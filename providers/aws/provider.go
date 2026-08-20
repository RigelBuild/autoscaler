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
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2_types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"go.woodpecker-ci.org/autoscaler/config"
	"go.woodpecker-ci.org/autoscaler/engine"
	"go.woodpecker-ci.org/autoscaler/engine/inits/cloudinit"
	"go.woodpecker-ci.org/autoscaler/engine/types"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

// EC2Client is the minimal subset of the EC2 API the provider calls. It exists
// so the deploy paths can be exercised with a generated mock; *ec2.Client
// satisfies it. It is exported so mockery emits an exported mock (mirroring the
// hetznercloud hcapi.Client precedent), which the table-driven tests consume.
type EC2Client interface {
	RunInstances(ctx context.Context, params *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	CreateFleet(ctx context.Context, params *ec2.CreateFleetInput, optFns ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error)
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

type provider struct {
	name                  string
	config                *config.Config
	instanceTypes         []string
	launchTemplateID      string
	amiID                 string
	tags                  []string
	region                string
	subnets               []string
	securityGroups        []string
	iamInstanceProfileArn string
	useSpotInstances      bool
	client                EC2Client
	lock                  sync.Mutex
	subnetRR              int
	sshKeyName            string
}

// resolveInstanceTypes applies the instance-type precedence: the plural
// aws-instance-types wins; otherwise the legacy singular aws-instance-type is
// used as a one-element list (when non-empty). More than one instance type
// requires a launch template id, since only the CreateFleet path can launch a
// diversified set.
func resolveInstanceTypes(instanceTypes []string, legacyInstanceType, launchTemplateID string) ([]string, error) {
	resolved := instanceTypes
	if len(resolved) == 0 && legacyInstanceType != "" {
		resolved = []string{legacyInstanceType}
	}
	if len(resolved) > 1 && launchTemplateID == "" {
		return nil, fmt.Errorf("aws-launch-template-id must be set when more than one aws-instance-types is configured")
	}
	if launchTemplateID != "" && len(resolved) == 0 {
		return nil, fmt.Errorf("aws-instance-types (or aws-instance-type) must be set when aws-launch-template-id is configured")
	}
	return resolved, nil
}

func New(ctx context.Context, c *cli.Command, config *config.Config) (types.Provider, error) {
	if len(c.StringSlice("aws-subnets")) == 0 {
		return nil, fmt.Errorf("aws-subnets must be set")
	}
	launchTemplateID := c.String("aws-launch-template-id")
	instanceTypes, err := resolveInstanceTypes(c.StringSlice("aws-instance-types"), c.String("aws-instance-type"), launchTemplateID)
	if err != nil {
		return nil, err
	}
	p := &provider{
		name:                  "aws",
		config:                config,
		instanceTypes:         instanceTypes,
		launchTemplateID:      launchTemplateID,
		amiID:                 c.String("aws-ami-id"),
		tags:                  c.StringSlice("aws-tags"),
		region:                c.String("aws-region"),
		subnets:               c.StringSlice("aws-subnets"),
		iamInstanceProfileArn: c.String("aws-iam-instance-profile-arn"),
		securityGroups:        c.StringSlice("aws-security-groups"),
		useSpotInstances:      c.Bool("aws-use-spot-instances"),
		sshKeyName:            c.String("aws-ssh-key-name"),
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(p.region))
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration, %w", err)
	}
	p.client = ec2.NewFromConfig(cfg)

	return p, nil
}

func (p *provider) DeployAgent(ctx context.Context, agent *woodpecker.Agent) error {
	userData, err := cloudinit.RenderUserDataTemplate(p.config, agent, cloudinit.RenderOption{})
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

	// A launch template anchors the CreateFleet path, which can span multiple
	// instance types. Without a template the original single-type RunInstances
	// path is used unchanged.
	if p.launchTemplateID != "" {
		return p.deployFleet(ctx, agent, encodedUserData, tags)
	}
	return p.deployRunInstances(ctx, agent, encodedUserData, tags)
}

func (p *provider) deployRunInstances(ctx context.Context, agent *woodpecker.Agent, encodedUserData *string, tags []ec2_types.Tag) error {
	var instanceType string
	if len(p.instanceTypes) > 0 {
		instanceType = p.instanceTypes[0]
	}

	runInstancesInput := ec2.RunInstancesInput{
		IamInstanceProfile: &ec2_types.IamInstanceProfileSpecification{
			Arn: aws.String(p.iamInstanceProfileArn),
		},
		ImageId:      aws.String(p.amiID),
		InstanceType: ec2_types.InstanceType(instanceType),
		MetadataOptions: &ec2_types.InstanceMetadataOptionsRequest{
			HttpEndpoint:            ec2_types.InstanceMetadataEndpointStateEnabled,
			HttpPutResponseHopLimit: aws.Int32(1),
			HttpTokens:              ec2_types.HttpTokensStateRequired,
		},
		SecurityGroupIds: p.securityGroups,
		MinCount:         aws.Int32(1),
		MaxCount:         aws.Int32(1),
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

	// When multiple subnets are given, assign agent to a subnet in a round-robin fashion.
	p.lock.Lock()
	runInstancesInput.SubnetId = aws.String(p.subnets[p.subnetRR])
	p.subnetRR = (p.subnetRR + 1) % len(p.subnets)
	p.lock.Unlock()

	if p.useSpotInstances {
		runInstancesInput.InstanceMarketOptions = &ec2_types.InstanceMarketOptionsRequest{
			MarketType: ec2_types.MarketTypeSpot,
		}
	}

	if p.sshKeyName != "" {
		runInstancesInput.KeyName = aws.String(p.sshKeyName)
	}

	runInstancesInput.UserData = encodedUserData
	result, err := p.client.RunInstances(ctx, &runInstancesInput)
	if err != nil {
		return fmt.Errorf("%s: RunInstances: %w", p.name, err)
	}

	return p.waitForAgent(ctx, agent, *result.Instances[0].InstanceId)
}

func (p *provider) deployFleet(ctx context.Context, agent *woodpecker.Agent, encodedUserData *string, tags []ec2_types.Tag) error {
	// IMDSv2-required metadata options, in the Fleet-prefixed request shape
	// (distinct from the RunInstances *InstanceMetadataOptionsRequest). Shared
	// read-only across every override.
	metadataOptions := &ec2_types.FleetInstanceMetadataOptionsRequest{
		HttpEndpoint:            ec2_types.FleetInstanceMetadataEndpointStateEnabled,
		HttpPutResponseHopLimit: aws.Int32(1),
		HttpTokens:              ec2_types.FleetHttpTokensStateRequired,
	}

	// Full cross-product of instance types x subnets. This supersedes the
	// RunInstances subnet round-robin: AWS picks the deepest-cheapest (type, AZ)
	// pool atomically. Security groups have no override field on an instant
	// fleet; they are sourced from the launch template.
	overrides := make([]ec2_types.FleetLaunchTemplateOverridesRequest, 0, len(p.instanceTypes)*len(p.subnets))
	for _, instanceType := range p.instanceTypes {
		for _, subnet := range p.subnets {
			override := ec2_types.FleetLaunchTemplateOverridesRequest{
				InstanceType: ec2_types.InstanceType(instanceType),
				SubnetId:     aws.String(subnet),
				ImageId:      aws.String(p.amiID),
				IamInstanceProfile: &ec2_types.FleetIamInstanceProfileSpecificationRequest{
					Arn: aws.String(p.iamInstanceProfileArn),
				},
				MetadataOptions: metadataOptions,
			}
			// Only attach a key pair when one is configured; aws.String("") is a
			// nonexistent key pair and fails every launch.
			if p.sshKeyName != "" {
				override.KeyName = aws.String(p.sshKeyName)
			}
			overrides = append(overrides, override)
		}
	}

	// The capacity type selects the market, mirroring the RunInstances
	// InstanceMarketOptions gate; omitting SpotOptions alone would still request
	// Spot, so both switch on useSpotInstances.
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

	out, err := p.client.CreateFleet(ctx, &createFleetInput)
	if err != nil {
		return fmt.Errorf("%s: CreateFleet: %w", p.name, err)
	}

	// instant fleets report per-launch failures in out.Errors with err == nil,
	// and a partial result can populate both Instances and Errors even at
	// capacity 1. The failure predicate is therefore an empty Instances list,
	// not a non-empty Errors list.
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
			return fmt.Errorf("failed to return list for agents")
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

func (p *provider) getAgent(ctx context.Context, agent *woodpecker.Agent) (*ec2_types.Instance, error) {
	instances, err := p.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2_types.Filter{
			{
				Name:   aws.String("tag:Name"),
				Values: []string{agent.Name},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(instances.Reservations) != 1 {
		return nil, fmt.Errorf("expected 1 reservation with tag:Name=%s, got %d", agent.Name, len(instances.Reservations))
	}
	if len(instances.Reservations[0].Instances) != 1 {
		return nil, fmt.Errorf("expected 1 instance with tag:Name=%s, got %d", agent.Name, len(instances.Reservations[0].Instances))
	}
	return &instances.Reservations[0].Instances[0], nil
}

func (p *provider) RemoveAgent(ctx context.Context, agent *woodpecker.Agent) error {
	instance, err := p.getAgent(ctx, agent)
	if err != nil {
		return err
	}

	_, err = p.client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{*instance.InstanceId},
	})
	return err
}

func (p *provider) ListDeployedAgentNames(ctx context.Context) ([]string, error) {
	log.Debug().Msgf("list deployed agent names")

	var names []string
	instances, err := p.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2_types.Filter{
			{
				Name:   aws.String(fmt.Sprintf("tag:%s", engine.LabelPool)),
				Values: []string{p.config.PoolID},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	for _, reservation := range instances.Reservations {
		for _, instance := range reservation.Instances {
			if instance.State.Name != ec2_types.InstanceStateNamePending &&
				instance.State.Name != ec2_types.InstanceStateNameRunning {
				continue
			}
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
