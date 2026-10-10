package aws

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2_types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"go.woodpecker-ci.org/autoscaler/config"
	"go.woodpecker-ci.org/autoscaler/providers/aws/ec2api/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

func newDeployTestProvider(client *mocks.MockClient, candidates []deployCandidate) *provider {
	p := newTestProvider(client)
	p.config = &config.Config{PoolID: "1"}
	p.deployCandidates = candidates
	for _, candidate := range candidates {
		if !slices.Contains(p.regions, candidate.regionConfig.region) {
			p.regions = append(p.regions, candidate.regionConfig.region)
		}
	}
	return p
}

func testCandidates() []deployCandidate {
	return []deployCandidate{
		{
			instanceType: testArmTypeInfo,
			regionConfig: regionConfig{
				region:         "eu-central-1",
				image:          testArmImage,
				subnets:        []string{"subnet-arm"},
				securityGroups: []string{"sg-arm"},
			},
		},
		{
			instanceType: testTypeInfo,
			regionConfig: regionConfig{
				region:         "us-east-1",
				image:          testImage,
				subnets:        []string{"subnet-x86"},
				securityGroups: []string{"sg-x86"},
			},
		},
	}
}

func mockAgentVisible(client *mocks.MockClient, agentName string) {
	client.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).
		Return(&ec2.DescribeInstancesOutput{
			Reservations: []ec2_types.Reservation{{Instances: []ec2_types.Instance{{
				InstanceId: aws.String("i-1"),
				State:      &ec2_types.InstanceState{Name: ec2_types.InstanceStateNameRunning},
				Tags:       []ec2_types.Tag{{Key: aws.String("Name"), Value: aws.String(agentName)}},
			}}}},
		}, nil)
}

func runRequest(instanceType, imageID, subnet, securityGroup string) func(*ec2.RunInstancesInput) bool {
	return func(in *ec2.RunInstancesInput) bool {
		return in.InstanceType == ec2_types.InstanceType(instanceType) &&
			aws.ToString(in.ImageId) == imageID &&
			aws.ToString(in.SubnetId) == subnet &&
			assert.ObjectsAreEqual([]string{securityGroup}, in.SecurityGroupIds)
	}
}

func TestDeployAgentFallback(t *testing.T) {
	agent := &woodpecker.Agent{Name: "pool-1-agent-abcd"}
	runOut := &ec2.RunInstancesOutput{Instances: []ec2_types.Instance{{InstanceId: aws.String("i-1")}}}

	t.Run("FirstCandidateSucceeds", func(t *testing.T) {
		client := mocks.NewMockClient(t)
		client.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).
			Return(runOut, nil).Once()
		mockAgentVisible(client, agent.Name)

		p := newDeployTestProvider(client, testCandidates())
		assert.NoError(t, p.DeployAgent(t.Context(), agent))
	})

	t.Run("CapacityErrorFallsBackToRegionalCandidate", func(t *testing.T) {
		client := mocks.NewMockClient(t)
		client.On("RunInstances", mock.Anything,
			mock.MatchedBy(runRequest("t4g.micro", "ami-arm", "subnet-arm", "sg-arm")),
			mock.MatchedBy(regionOptions("eu-central-1"))).
			Return(nil, &apiError{code: "InsufficientInstanceCapacity"}).Once()
		client.On("RunInstances", mock.Anything,
			mock.MatchedBy(runRequest("m6i.large", "ami-x86", "subnet-x86", "sg-x86")),
			mock.MatchedBy(regionOptions("us-east-1"))).
			Return(runOut, nil).Once()
		mockAgentVisible(client, agent.Name)

		p := newDeployTestProvider(client, testCandidates())
		assert.NoError(t, p.DeployAgent(t.Context(), agent))
	})

	t.Run("NonCapacityErrorAborts", func(t *testing.T) {
		client := mocks.NewMockClient(t)
		client.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).
			Return(nil, &apiError{code: "InvalidSubnetID.NotFound"}).Once()

		p := newDeployTestProvider(client, testCandidates())
		err := p.DeployAgent(t.Context(), agent)
		assert.ErrorContains(t, err, "InvalidSubnetID.NotFound")
	})

	t.Run("EmptyRunInstancesResult", func(t *testing.T) {
		client := mocks.NewMockClient(t)
		client.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).
			Return(&ec2.RunInstancesOutput{}, nil).Once()

		p := newDeployTestProvider(client, testCandidates())
		err := p.DeployAgent(t.Context(), agent)
		assert.ErrorContains(t, err, "returned no instances")
	})

	t.Run("AllCandidatesOutOfCapacity", func(t *testing.T) {
		client := mocks.NewMockClient(t)
		client.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).
			Return(nil, &apiError{code: "InsufficientInstanceCapacity"}).Twice()

		p := newDeployTestProvider(client, testCandidates())
		err := p.DeployAgent(t.Context(), agent)
		assert.ErrorContains(t, err, "all 2 deploy candidates out of capacity")
	})
}

func TestRemoveAgentSkipsOSShutdown(t *testing.T) {
	agent := &woodpecker.Agent{Name: "pool-1-agent-abcd"}

	client := mocks.NewMockClient(t)
	mockAgentVisible(client, agent.Name)
	client.On("TerminateInstances", mock.Anything,
		mock.MatchedBy(func(in *ec2.TerminateInstancesInput) bool {
			return assert.ObjectsAreEqual([]string{"i-1"}, in.InstanceIds) &&
				aws.ToBool(in.SkipOsShutdown)
		}),
		mock.Anything).
		Return(&ec2.TerminateInstancesOutput{}, nil).Once()

	p := newTestProvider(client)
	p.regions = []string{"eu-central-1"}
	assert.NoError(t, p.RemoveAgent(t.Context(), agent))
}

// fleetCandidates is two arm64 types in one region sharing the given subnets.
func fleetCandidates(subnets ...string) []deployCandidate {
	c6g := testArmTypeInfo
	c6g.InstanceType = "c6g.large"
	candidates := make([]deployCandidate, 0, 2)
	for _, it := range []ec2_types.InstanceTypeInfo{testArmTypeInfo, c6g} {
		candidates = append(candidates, deployCandidate{
			instanceType: it,
			regionConfig: regionConfig{
				region:         "eu-central-1",
				image:          testArmImage,
				subnets:        subnets,
				securityGroups: []string{"sg-arm"},
			},
		})
	}
	return candidates
}

func fleetOut(errs ...ec2_types.CreateFleetError) *ec2.CreateFleetOutput {
	return &ec2.CreateFleetOutput{
		Instances: []ec2_types.CreateFleetInstance{{InstanceIds: []string{"i-1"}}},
		Errors:    errs,
	}
}

var errFleetAPI = errors.New("UnauthorizedOperation")

func TestDeployAgentFleet(t *testing.T) {
	agent := &woodpecker.Agent{Name: "pool-1-agent-abcd"}

	tests := []struct {
		name             string
		launchTemplateID string
		useSpotInstances bool
		sshKeyName       string
		setupMocks       func(*mocks.MockClient)
		expectedError    string
		wantErrIs        error
	}{
		{
			name: "NoTemplateUsesRunInstances",
			setupMocks: func(client *mocks.MockClient) {
				client.On("RunInstances", mock.Anything,
					mock.MatchedBy(runRequest("t4g.micro", "ami-arm", "subnet-a", "sg-arm")),
					mock.MatchedBy(regionOptions("eu-central-1"))).
					Return(&ec2.RunInstancesOutput{Instances: []ec2_types.Instance{{InstanceId: aws.String("i-1")}}}, nil).Once()
				mockAgentVisible(client, agent.Name)
				// CreateFleet is unregistered, so the mock fails the test if it is called.
			},
		},
		{
			name:             "SpotFleetWithFullOverrideCrossProduct",
			launchTemplateID: "lt-abc",
			useSpotInstances: true,
			setupMocks: func(client *mocks.MockClient) {
				client.On("CreateFleet", mock.Anything, mock.MatchedBy(func(in *ec2.CreateFleetInput) bool {
					if in.Type != ec2_types.FleetTypeInstant ||
						in.SpotOptions == nil ||
						in.SpotOptions.AllocationStrategy != ec2_types.SpotAllocationStrategyPriceCapacityOptimized ||
						in.TargetCapacitySpecification == nil ||
						in.TargetCapacitySpecification.DefaultTargetCapacityType != ec2_types.DefaultTargetCapacityTypeSpot ||
						aws.ToInt32(in.TargetCapacitySpecification.TotalTargetCapacity) != 1 ||
						len(in.LaunchTemplateConfigs) != 1 ||
						len(in.TagSpecifications) != 2 {
						return false
					}
					cfg := in.LaunchTemplateConfigs[0]
					if cfg.LaunchTemplateSpecification == nil ||
						aws.ToString(cfg.LaunchTemplateSpecification.LaunchTemplateId) != "lt-abc" ||
						aws.ToString(cfg.LaunchTemplateSpecification.Version) != "$Default" ||
						aws.ToString(cfg.LaunchTemplateSpecification.LaunchTemplateSpecificationUserData) == "" ||
						len(cfg.Overrides) != 4 {
						return false
					}
					seen := map[string]bool{}
					for _, o := range cfg.Overrides {
						if o.MetadataOptions == nil ||
							o.MetadataOptions.HttpTokens != ec2_types.FleetHttpTokensStateRequired ||
							o.MetadataOptions.HttpEndpoint != ec2_types.FleetInstanceMetadataEndpointStateEnabled ||
							aws.ToInt32(o.MetadataOptions.HttpPutResponseHopLimit) != 1 ||
							aws.ToString(o.ImageId) != "ami-arm" ||
							o.IamInstanceProfile == nil ||
							aws.ToString(o.IamInstanceProfile.Arn) != "arn:profile" ||
							o.KeyName != nil {
							return false
						}
						seen[string(o.InstanceType)+"|"+aws.ToString(o.SubnetId)] = true
					}
					return seen["t4g.micro|subnet-a"] && seen["t4g.micro|subnet-b"] &&
						seen["c6g.large|subnet-a"] && seen["c6g.large|subnet-b"]
				}), mock.MatchedBy(regionOptions("eu-central-1"))).Return(fleetOut(), nil).Once()
				mockAgentVisible(client, agent.Name)
			},
		},
		{
			name:             "OnDemandFleetWhenSpotDisabled",
			launchTemplateID: "lt-abc",
			setupMocks: func(client *mocks.MockClient) {
				client.On("CreateFleet", mock.Anything, mock.MatchedBy(func(in *ec2.CreateFleetInput) bool {
					return in.SpotOptions == nil &&
						in.TargetCapacitySpecification != nil &&
						in.TargetCapacitySpecification.DefaultTargetCapacityType == ec2_types.DefaultTargetCapacityTypeOnDemand
				}), mock.Anything).Return(fleetOut(), nil).Once()
				mockAgentVisible(client, agent.Name)
			},
		},
		{
			name:             "KeyNameAttachedWhenConfigured",
			launchTemplateID: "lt-abc",
			sshKeyName:       "my-key",
			setupMocks: func(client *mocks.MockClient) {
				client.On("CreateFleet", mock.Anything, mock.MatchedBy(func(in *ec2.CreateFleetInput) bool {
					for _, o := range in.LaunchTemplateConfigs[0].Overrides {
						if aws.ToString(o.KeyName) != "my-key" {
							return false
						}
					}
					return true
				}), mock.Anything).Return(fleetOut(), nil).Once()
				mockAgentVisible(client, agent.Name)
			},
		},
		{
			name:             "PartialResultWithInstancesAndErrorsSucceeds",
			launchTemplateID: "lt-abc",
			useSpotInstances: true,
			setupMocks: func(client *mocks.MockClient) {
				client.On("CreateFleet", mock.Anything, mock.Anything, mock.Anything).
					Return(fleetOut(ec2_types.CreateFleetError{
						ErrorCode:    aws.String("InsufficientInstanceCapacity"),
						ErrorMessage: aws.String("no capacity for t4g.micro"),
					}), nil).Once()
				mockAgentVisible(client, agent.Name)
			},
		},
		{
			name:             "ErrorsOnlyResultReturnsAWSCode",
			launchTemplateID: "lt-abc",
			useSpotInstances: true,
			setupMocks: func(client *mocks.MockClient) {
				client.On("CreateFleet", mock.Anything, mock.Anything, mock.Anything).
					Return(&ec2.CreateFleetOutput{Errors: []ec2_types.CreateFleetError{{
						ErrorCode:    aws.String("InsufficientInstanceCapacity"),
						ErrorMessage: aws.String("no Spot capacity available"),
					}}}, nil).Once()
			},
			expectedError: "InsufficientInstanceCapacity: no Spot capacity available",
		},
		{
			name:             "APIErrorIsWrapped",
			launchTemplateID: "lt-abc",
			setupMocks: func(client *mocks.MockClient) {
				client.On("CreateFleet", mock.Anything, mock.Anything, mock.Anything).
					Return(nil, errFleetAPI).Once()
			},
			expectedError: "CreateFleet: UnauthorizedOperation",
			wantErrIs:     errFleetAPI,
		},
		{
			name:             "EmptyResultWithoutErrorsFails",
			launchTemplateID: "lt-abc",
			setupMocks: func(client *mocks.MockClient) {
				client.On("CreateFleet", mock.Anything, mock.Anything, mock.Anything).
					Return(&ec2.CreateFleetOutput{}, nil).Once()
			},
			expectedError: "CreateFleet: no instances launched",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := mocks.NewMockClient(t)
			tt.setupMocks(client)

			candidates := fleetCandidates("subnet-a", "subnet-b")
			if tt.launchTemplateID == "" {
				candidates = candidates[:1]
			}
			p := newDeployTestProvider(client, candidates)
			p.launchTemplateID = tt.launchTemplateID
			p.useSpotInstances = tt.useSpotInstances
			p.sshKeyName = tt.sshKeyName
			p.iamInstanceProfileArn = "arn:profile"

			err := p.DeployAgent(t.Context(), agent)
			if tt.expectedError != "" {
				assert.ErrorContains(t, err, tt.expectedError)
				if tt.wantErrIs != nil {
					assert.ErrorIs(t, err, tt.wantErrIs)
				}
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestValidateLaunchTemplate(t *testing.T) {
	t.Run("MultiRegionRejected", func(t *testing.T) {
		p := newDeployTestProvider(mocks.NewMockClient(t), testCandidates())
		p.launchTemplateID = "lt-abc"
		assert.ErrorIs(t, p.validateLaunchTemplate(), ErrLaunchTemplateMultiRegion)
	})

	t.Run("SingleRegionAllowed", func(t *testing.T) {
		p := newDeployTestProvider(mocks.NewMockClient(t), fleetCandidates("subnet-a"))
		p.launchTemplateID = "lt-abc"
		assert.NoError(t, p.validateLaunchTemplate())
	})

	t.Run("MultiRegionWithoutTemplateAllowed", func(t *testing.T) {
		p := newDeployTestProvider(mocks.NewMockClient(t), testCandidates())
		assert.NoError(t, p.validateLaunchTemplate())
	})
}

func TestInstanceTypesEnvPopulatesInstanceTypeFlag(t *testing.T) {
	// The diversified plural list must win over a leftover legacy singular.
	t.Setenv("WOODPECKER_AWS_INSTANCE_TYPE", "t3.micro")
	t.Setenv("WOODPECKER_AWS_INSTANCE_TYPES", "t4g.medium,c6g.large")

	cmd := &cli.Command{
		Flags: ProviderFlags,
		Action: func(_ context.Context, cmd *cli.Command) error {
			assert.Equal(t, []string{"t4g.medium", "c6g.large"}, cmd.StringSlice("aws-instance-type"))
			return nil
		},
	}

	require.NoError(t, cmd.Run(t.Context(), []string{"autoscaler"}))
}
