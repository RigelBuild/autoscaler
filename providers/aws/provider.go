package aws

import (
	"context"
	b64 "encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2_types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"go.woodpecker-ci.org/autoscaler/config"
	"go.woodpecker-ci.org/autoscaler/engine"
	"go.woodpecker-ci.org/autoscaler/engine/inits/cloudinit"
	"go.woodpecker-ci.org/autoscaler/engine/types"
	"go.woodpecker-ci.org/autoscaler/providers/aws/ec2api"
	"go.woodpecker-ci.org/autoscaler/utils"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

// blackhole metadata services so running steps can not extract agent token from user-data
// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-service.html
var blackholeMetadataAPI = []string{
	"ip -4 route add blackhole 169.254.169.254/32",
	"ip -6 route add blackhole fd00:ec2::254/128",
}

type provider struct {
	name                  string
	config                *config.Config
	tags                  []string
	region                string
	iamInstanceProfileArn string
	useSpotInstances      bool
	client                ec2api.Client
	lock                  sync.Mutex
	subnetRR              int
	sshKeyName            string
	launchTemplateID      string
	// resolved config
	deployCandidates []deployCandidate
	regions          []string
}

func New(ctx context.Context, c *cli.Command, config *config.Config) (types.Provider, error) {
	p := &provider{
		name:                  "aws",
		config:                config,
		tags:                  c.StringSlice("aws-tags"),
		region:                c.String("aws-region"),
		iamInstanceProfileArn: c.String("aws-iam-instance-profile-arn"),
		useSpotInstances:      c.Bool("aws-use-spot-instances"),
		sshKeyName:            c.String("aws-ssh-key-name"),
		launchTemplateID:      c.String("aws-launch-template-id"),
	}
	if err := utils.CheckReservedTags(p.tags, engine.LabelPrefix, ErrReservedTagPrefix); err != nil {
		return nil, fmt.Errorf("%s: %w", p.name, err)
	}

	loadOptions := []func(*awsconfig.LoadOptions) error{}
	credentialsOption, err := staticCredentialsOption(
		c.String("aws-access-key-id"),
		c.String("aws-secret-access-key"),
	)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.name, err)
	}
	if credentialsOption != nil {
		loadOptions = append(loadOptions, credentialsOption)
	}
	if p.region != "" {
		loadOptions = append(loadOptions, awsconfig.WithRegion(p.region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration, %w", err)
	}
	if p.region == "" {
		// Fall back to the region the SDK resolved from its default chain
		// (AWS_REGION, shared config, IMDS) so unqualified values keep
		// working without the aws-region flag.
		p.region = cfg.Region
	}
	instanceTypes := c.StringSlice("aws-instance-type")
	authenticationRegion, err := p.authenticationRegion(instanceTypes)
	if err != nil {
		return nil, err
	}
	identityClient := sts.NewFromConfig(cfg, func(options *sts.Options) {
		options.Region = authenticationRegion
	})
	if err := authenticateAWS(ctx, identityClient); err != nil {
		return nil, fmt.Errorf("%s: %w", p.name, err)
	}
	p.client = ec2.NewFromConfig(cfg)

	if err := p.resolveDeployCandidates(
		ctx,
		instanceTypes,
		c.String("aws-ami-id"),
		c.StringSlice("aws-subnets"),
		c.StringSlice("aws-security-groups"),
	); err != nil {
		return nil, err
	}
	if err := p.validateLaunchTemplate(); err != nil {
		return nil, err
	}

	p.printResolvedConfig()

	return p, nil
}

func (p *provider) authenticationRegion(instanceTypes []string) (string, error) {
	if p.region != "" {
		return p.region, nil
	}
	if len(instanceTypes) == 0 {
		return "", fmt.Errorf("%s: %w", p.name, ErrNoDeployCandidates)
	}

	_, region, err := p.valueRegion("aws-instance-type", instanceTypes[0])
	return region, err
}

// validateLaunchTemplate rejects a launch template with candidates in more
// than one region: CreateFleet is a regional call, and a launch template id
// only exists in one region.
func (p *provider) validateLaunchTemplate() error {
	if p.launchTemplateID == "" || len(p.regions) <= 1 {
		return nil
	}
	return fmt.Errorf("%s: %w: candidates span %v", p.name, ErrLaunchTemplateMultiRegion, p.regions)
}

type identityClient interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

func authenticateAWS(ctx context.Context, client identityClient) error {
	identity, err := client.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("authenticate with AWS: %w", err)
	}

	log.Info().
		Str("account", aws.ToString(identity.Account)).
		Str("arn", aws.ToString(identity.Arn)).
		Msg("authenticated with AWS")
	return nil
}

func staticCredentialsOption(accessKeyID, secretAccessKey string) (func(*awsconfig.LoadOptions) error, error) {
	if accessKeyID == "" && secretAccessKey == "" {
		return nil, nil
	}
	if accessKeyID == "" {
		return nil, fmt.Errorf("aws-access-key-id must be set when aws-secret-access-key is set")
	}
	if secretAccessKey == "" {
		return nil, fmt.Errorf("aws-secret-access-key must be set when aws-access-key-id is set")
	}

	return awsconfig.WithCredentialsProvider(
		credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
	), nil
}

func (p *provider) printResolvedConfig() {
	for _, c := range p.deployCandidates {
		log.Info().
			Str("type", string(c.instanceType.InstanceType)).
			Str("region", c.regionConfig.region).
			Str("ami", aws.ToString(c.regionConfig.image.ImageId)).
			Str("ami_arch", string(c.regionConfig.image.Architecture)).
			Bool("current_gen", aws.ToBool(c.instanceType.CurrentGeneration)).
			Msg("deploy candidate")
	}
}

func (p *provider) DeployAgent(ctx context.Context, agent *woodpecker.Agent) error {
	userData, err := cloudinit.RenderUserDataTemplate(p.config, agent, cloudinit.RenderOption{
		PreExec: blackholeMetadataAPI,
	})
	if err != nil {
		return fmt.Errorf("%s: cloudinit.RenderUserDataTemplate: %w", p.name, err)
	}

	// Generate base tags for instance
	tags := []ec2_types.Tag{{
		Key:   aws.String("Name"),
		Value: aws.String(agent.Name),
	}, {
		Key:   aws.String(engine.LabelPool),
		Value: aws.String(p.config.PoolID),
	}}

	// Append user specified tags
	tagKVParts := 2
	for _, tag := range p.tags {
		parts := strings.Split(tag, "=")
		var rt ec2_types.Tag
		if len(parts) >= tagKVParts {
			rt = ec2_types.Tag{
				Key:   aws.String(parts[0]),
				Value: aws.String(parts[1]),
			}
		} else {
			rt = ec2_types.Tag{
				Key: aws.String(parts[0]),
			}
		}

		tags = append(tags, rt)
	}

	encodedUserData := aws.String(b64.StdEncoding.EncodeToString([]byte(userData)))

	// A launch template anchors the CreateFleet path, which lets AWS choose
	// across every candidate type and subnet in one call.
	if p.launchTemplateID != "" {
		return p.deployFleet(ctx, agent, encodedUserData, tags)
	}

	runInstancesInput := ec2.RunInstancesInput{
		IamInstanceProfile: &ec2_types.IamInstanceProfileSpecification{
			Arn: aws.String(p.iamInstanceProfileArn),
		},
		MetadataOptions: &ec2_types.InstanceMetadataOptionsRequest{
			HttpEndpoint:            ec2_types.InstanceMetadataEndpointStateEnabled,
			HttpPutResponseHopLimit: aws.Int32(1),
			HttpTokens:              ec2_types.HttpTokensStateRequired,
		},
		MinCount: aws.Int32(1),
		MaxCount: aws.Int32(1),
		TagSpecifications: []ec2_types.TagSpecification{
			{
				ResourceType: "instance",
				Tags:         tags,
			},
			{
				ResourceType: "volume",
				Tags:         tags,
			},
		},
	}

	if p.useSpotInstances {
		runInstancesInput.InstanceMarketOptions = &ec2_types.InstanceMarketOptionsRequest{
			MarketType: ec2_types.MarketTypeSpot,
		}
	}

	if p.sshKeyName != "" {
		runInstancesInput.KeyName = aws.String(p.sshKeyName)
	}

	runInstancesInput.UserData = encodedUserData

	var result *ec2.RunInstancesOutput
	for i, c := range p.deployCandidates {
		runInstancesInput.InstanceType = c.instanceType.InstanceType
		runInstancesInput.ImageId = c.regionConfig.image.ImageId
		runInstancesInput.SecurityGroupIds = c.regionConfig.securityGroups

		// When multiple subnets are given, assign agent to a subnet in a round-robin fashion.
		p.lock.Lock()
		runInstancesInput.SubnetId = aws.String(c.regionConfig.subnets[p.subnetRR%len(c.regionConfig.subnets)])
		p.subnetRR = (p.subnetRR + 1) % len(c.regionConfig.subnets)
		p.lock.Unlock()

		log.Info().
			Str("type", string(c.instanceType.InstanceType)).
			Str("region", c.regionConfig.region).
			Msg("create agent")

		result, err = p.client.RunInstances(ctx, &runInstancesInput, regionOpt(c.regionConfig.region))
		if err == nil {
			break
		}

		// Continue to next fallback entry only if capacity is unavailable.
		if !isCapacityError(err) {
			return fmt.Errorf("%s: RunInstances: %w", p.name, err)
		}

		// Only log and continue if there are more candidates left.
		if i < len(p.deployCandidates)-1 {
			log.Warn().Msgf(
				"create agent failed: type = %s region = %s: %s",
				c.instanceType.InstanceType, c.regionConfig.region, err,
			)
			continue
		}

		// Last candidate failed: the whole fallback chain is exhausted.
		return fmt.Errorf("%s: all %d deploy candidates out of capacity, last: RunInstances: %w",
			p.name, len(p.deployCandidates), err)
	}

	if result == nil || len(result.Instances) == 0 {
		return fmt.Errorf("%s: RunInstances returned no instances", p.name)
	}

	return p.waitForAgent(ctx, agent, *result.Instances[0].InstanceId)
}

func (p *provider) deployFleet(ctx context.Context, agent *woodpecker.Agent, encodedUserData *string, tags []ec2_types.Tag) error {
	// IMDSv2-required metadata options in the Fleet request shape, shared
	// read-only across every override.
	metadataOptions := &ec2_types.FleetInstanceMetadataOptionsRequest{
		HttpEndpoint:            ec2_types.FleetInstanceMetadataEndpointStateEnabled,
		HttpPutResponseHopLimit: aws.Int32(1),
		HttpTokens:              ec2_types.FleetHttpTokensStateRequired,
	}

	// Full cross-product of candidates x their subnets, replacing the subnet
	// round-robin. Security groups have no override field on an instant fleet;
	// they come from the launch template.
	overrides := make([]ec2_types.FleetLaunchTemplateOverridesRequest, 0, len(p.deployCandidates))
	for _, c := range p.deployCandidates {
		for _, subnet := range c.regionConfig.subnets {
			override := ec2_types.FleetLaunchTemplateOverridesRequest{
				InstanceType: c.instanceType.InstanceType,
				SubnetId:     aws.String(subnet),
				ImageId:      c.regionConfig.image.ImageId,
				IamInstanceProfile: &ec2_types.FleetIamInstanceProfileSpecificationRequest{
					Arn: aws.String(p.iamInstanceProfileArn),
				},
				MetadataOptions: metadataOptions,
			}
			// aws.String("") names a nonexistent key pair and fails every launch.
			if p.sshKeyName != "" {
				override.KeyName = aws.String(p.sshKeyName)
			}
			overrides = append(overrides, override)
		}
	}

	// The capacity type selects the market; omitting SpotOptions alone would
	// still request Spot, so both switch on useSpotInstances.
	targetCapacityType := ec2_types.DefaultTargetCapacityTypeOnDemand
	if p.useSpotInstances {
		targetCapacityType = ec2_types.DefaultTargetCapacityTypeSpot
	}

	createFleetInput := ec2.CreateFleetInput{
		Type: ec2_types.FleetTypeInstant,
		TargetCapacitySpecification: &ec2_types.TargetCapacitySpecificationRequest{
			TotalTargetCapacity:       aws.Int32(1),
			DefaultTargetCapacityType: targetCapacityType,
		},
		LaunchTemplateConfigs: []ec2_types.FleetLaunchTemplateConfigRequest{
			{
				LaunchTemplateSpecification: &ec2_types.FleetLaunchTemplateSpecificationRequest{
					LaunchTemplateId:                    aws.String(p.launchTemplateID),
					Version:                             aws.String("$Default"),
					LaunchTemplateSpecificationUserData: encodedUserData,
				},
				Overrides: overrides,
			},
		},
		TagSpecifications: []ec2_types.TagSpecification{
			{
				ResourceType: "instance",
				Tags:         tags,
			},
			{
				ResourceType: "volume",
				Tags:         tags,
			},
		},
	}

	if p.useSpotInstances {
		createFleetInput.SpotOptions = &ec2_types.SpotOptionsRequest{
			AllocationStrategy: ec2_types.SpotAllocationStrategyPriceCapacityOptimized,
		}
	}

	// New rejects a launch template across regions, so all candidates share one.
	region := p.deployCandidates[0].regionConfig.region
	log.Info().
		Int("overrides", len(overrides)).
		Str("region", region).
		Msg("create agent via fleet")

	out, err := p.client.CreateFleet(ctx, &createFleetInput, regionOpt(region))
	if err != nil {
		return fmt.Errorf("%s: CreateFleet: %w", p.name, err)
	}

	// Instant fleets report per-launch failures in out.Errors with err == nil,
	// and a partial result can carry both Instances and Errors. Failure is
	// therefore an empty Instances list, not a non-empty Errors list.
	if len(out.Instances) == 0 || len(out.Instances[0].InstanceIds) == 0 {
		if len(out.Errors) > 0 {
			return fmt.Errorf("%s: CreateFleet: %s: %s", p.name,
				aws.ToString(out.Errors[0].ErrorCode), aws.ToString(out.Errors[0].ErrorMessage))
		}
		return fmt.Errorf("%s: CreateFleet: no instances launched", p.name)
	}

	return p.waitForAgent(ctx, agent, out.Instances[0].InstanceIds[0])
}

func (p *provider) waitForAgent(ctx context.Context, agent *woodpecker.Agent, instanceID string) error {
	// Wait until instance is available. Sometimes it can take a second or two for the tag based
	// filter to show the instance we just created in AWS
	log.Debug().Msgf("waiting for instance %s", instanceID)
	for range 5 {
		agents, err := p.ListDeployedAgentNames(ctx)
		if err != nil {
			return fmt.Errorf("%s: ListDeployedAgentNames: %w", p.name, err)
		}

		for _, a := range agents {
			if a == agent.Name {
				return nil
			}
		}

		log.Debug().Msgf("created agent not found in list yet")
		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("instance did not resolve in agent list: %s", instanceID)
}

func (p *provider) RemoveAgent(ctx context.Context, agent *woodpecker.Agent) error {
	instance, region, err := p.getAgent(ctx, agent)
	if err != nil {
		return err
	}

	_, err = p.client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{*instance.InstanceId},
		// Skip the graceful OS shutdown so an unresponsive or hung guest
		// cannot block termination and leave a dangling instance behind.
		SkipOsShutdown: aws.Bool(true),
	}, regionOpt(region))
	return err
}

func (p *provider) ListDeployedAgentNames(ctx context.Context) ([]string, error) {
	log.Debug().Msgf("list deployed agent names")

	var names []string
	for _, region := range p.regions {
		instances, err := p.instancesByTag(ctx, region, engine.LabelPool, p.config.PoolID)
		if err != nil {
			return nil, err
		}
		for _, instance := range instances {
			for _, tag := range instance.Tags {
				if *tag.Key == "Name" {
					log.Debug().Msgf("found agent %s", *tag.Value)
					names = append(names, *tag.Value)
				}
			}
		}
	}
	return names, nil
}

func (p *provider) BillingModel() types.BillingModel {
	return types.BillingPerSecond
}
