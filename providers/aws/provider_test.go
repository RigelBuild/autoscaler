package aws

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2_types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/autoscaler/config"
	"go.woodpecker-ci.org/autoscaler/providers/aws/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

func TestResolveInstanceTypes(t *testing.T) {
	tests := []struct {
		name               string
		instanceTypes      []string
		legacyInstanceType string
		launchTemplateID   string
		want               []string
		wantErr            bool
	}{
		{
			name:               "singular back-compat resolves to one-element list",
			legacyInstanceType: "t4g.medium",
			want:               []string{"t4g.medium"},
		},
		{
			name:          "plural wins over legacy",
			instanceTypes: []string{"c6g.large", "c7g.large"},
			// legacy set but ignored because plural is present.
			legacyInstanceType: "t4g.medium",
			launchTemplateID:   "lt-123",
			want:               []string{"c6g.large", "c7g.large"},
		},
		{
			name:             "single plural type without template id is allowed",
			instanceTypes:    []string{"t4g.medium"},
			launchTemplateID: "",
			want:             []string{"t4g.medium"},
		},
		{
			name:          "multi-type without template id errors",
			instanceTypes: []string{"t4g.medium", "c6g.large"},
			wantErr:       true,
		},
		{
			name:             "multi-type with template id ok",
			instanceTypes:    []string{"t4g.medium", "c6g.large"},
			launchTemplateID: "lt-123",
			want:             []string{"t4g.medium", "c6g.large"},
		},
		{
			name:             "template id without any instance type errors",
			launchTemplateID: "lt-123",
			wantErr:          true,
		},
		{
			name: "nothing configured resolves to empty",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveInstanceTypes(tt.instanceTypes, tt.legacyInstanceType, tt.launchTemplateID)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// testAgentName is the pool agent every deploy case expects back.
const testAgentName = "pool-1-agent-abc"

// describeReturnsAgent makes DescribeInstances report testAgentName in the pool
// so the deploy path's wait loop resolves on its first poll (no clock-based
// waiting in the test).
func describeReturnsAgent(mockClient *mocks.MockEC2Client) {
	mockClient.On("DescribeInstances", mock.Anything, mock.Anything).Return(&ec2.DescribeInstancesOutput{
		Reservations: []ec2_types.Reservation{
			{
				Instances: []ec2_types.Instance{
					{
						State: &ec2_types.InstanceState{Name: ec2_types.InstanceStateNameRunning},
						Tags: []ec2_types.Tag{
							{Key: aws.String("Name"), Value: aws.String(testAgentName)},
						},
					},
				},
			},
		},
	}, nil)
}

func TestDeployAgent(t *testing.T) {
	const agentName = testAgentName

	tests := []struct {
		name             string
		instanceTypes    []string
		launchTemplateID string
		subnets          []string
		useSpotInstances bool
		sshKeyName       string
		setupMocks       func(*mocks.MockEC2Client)
		expectedError    string
	}{
		{
			name:          "no template uses RunInstances not CreateFleet",
			instanceTypes: []string{"t4g.medium"},
			subnets:       []string{"subnet-a"},
			setupMocks: func(mockClient *mocks.MockEC2Client) {
				mockClient.On("RunInstances", mock.Anything, mock.MatchedBy(func(in *ec2.RunInstancesInput) bool {
					return in.InstanceType == "t4g.medium" &&
						aws.ToString(in.SubnetId) == "subnet-a"
				})).Return(&ec2.RunInstancesOutput{
					Instances: []ec2_types.Instance{{InstanceId: aws.String("i-1")}},
				}, nil).Once()
				describeReturnsAgent(mockClient)
				// CreateFleet must never be called on this path; leaving it
				// unregistered means the mock fails the test if it is.
			},
		},
		{
			name:             "multi-type spot fleet with full override cross-product",
			instanceTypes:    []string{"t4g.medium", "c6g.large"},
			launchTemplateID: "lt-abc",
			subnets:          []string{"subnet-a", "subnet-b"},
			useSpotInstances: true,
			setupMocks: func(mockClient *mocks.MockEC2Client) {
				mockClient.On("CreateFleet", mock.Anything, mock.MatchedBy(func(in *ec2.CreateFleetInput) bool {
					if in.Type != ec2_types.FleetTypeInstant {
						return false
					}
					if in.SpotOptions == nil ||
						in.SpotOptions.AllocationStrategy != ec2_types.SpotAllocationStrategyPriceCapacityOptimized {
						return false
					}
					if in.TargetCapacitySpecification == nil ||
						in.TargetCapacitySpecification.DefaultTargetCapacityType != ec2_types.DefaultTargetCapacityTypeSpot ||
						aws.ToInt32(in.TargetCapacitySpecification.TotalTargetCapacity) != 1 {
						return false
					}
					if len(in.LaunchTemplateConfigs) != 1 {
						return false
					}
					cfg := in.LaunchTemplateConfigs[0]
					if cfg.LaunchTemplateSpecification == nil ||
						aws.ToString(cfg.LaunchTemplateSpecification.LaunchTemplateId) != "lt-abc" ||
						aws.ToString(cfg.LaunchTemplateSpecification.Version) != "$Default" ||
						aws.ToString(cfg.LaunchTemplateSpecification.LaunchTemplateSpecificationUserData) == "" {
						return false
					}
					// 2 types x 2 subnets = 4 overrides.
					if len(cfg.Overrides) != 4 {
						return false
					}
					// Every override carries IMDSv2-required Fleet metadata, the
					// image, profile, and no key name (none configured), and no
					// security groups (the type has no such field).
					seen := map[string]bool{}
					for _, o := range cfg.Overrides {
						if o.MetadataOptions == nil ||
							o.MetadataOptions.HttpTokens != ec2_types.FleetHttpTokensStateRequired ||
							o.MetadataOptions.HttpEndpoint != ec2_types.FleetInstanceMetadataEndpointStateEnabled ||
							aws.ToInt32(o.MetadataOptions.HttpPutResponseHopLimit) != 1 {
							return false
						}
						if o.IamInstanceProfile == nil || o.KeyName != nil {
							return false
						}
						seen[string(o.InstanceType)+"|"+aws.ToString(o.SubnetId)] = true
					}
					return seen["t4g.medium|subnet-a"] && seen["t4g.medium|subnet-b"] &&
						seen["c6g.large|subnet-a"] && seen["c6g.large|subnet-b"]
				})).Return(&ec2.CreateFleetOutput{
					Instances: []ec2_types.CreateFleetInstance{{InstanceIds: []string{"i-9"}}},
				}, nil).Once()
				describeReturnsAgent(mockClient)
			},
		},
		{
			name:             "on-demand fleet when spot disabled",
			instanceTypes:    []string{"t4g.medium", "c6g.large"},
			launchTemplateID: "lt-abc",
			subnets:          []string{"subnet-a"},
			useSpotInstances: false,
			setupMocks: func(mockClient *mocks.MockEC2Client) {
				mockClient.On("CreateFleet", mock.Anything, mock.MatchedBy(func(in *ec2.CreateFleetInput) bool {
					return in.SpotOptions == nil &&
						in.TargetCapacitySpecification != nil &&
						in.TargetCapacitySpecification.DefaultTargetCapacityType == ec2_types.DefaultTargetCapacityTypeOnDemand
				})).Return(&ec2.CreateFleetOutput{
					Instances: []ec2_types.CreateFleetInstance{{InstanceIds: []string{"i-9"}}},
				}, nil).Once()
				describeReturnsAgent(mockClient)
			},
		},
		{
			name:             "key name attached to overrides when configured",
			instanceTypes:    []string{"t4g.medium"},
			launchTemplateID: "lt-abc",
			subnets:          []string{"subnet-a"},
			sshKeyName:       "my-key",
			setupMocks: func(mockClient *mocks.MockEC2Client) {
				mockClient.On("CreateFleet", mock.Anything, mock.MatchedBy(func(in *ec2.CreateFleetInput) bool {
					for _, o := range in.LaunchTemplateConfigs[0].Overrides {
						if aws.ToString(o.KeyName) != "my-key" {
							return false
						}
					}
					return true
				})).Return(&ec2.CreateFleetOutput{
					Instances: []ec2_types.CreateFleetInstance{{InstanceIds: []string{"i-9"}}},
				}, nil).Once()
				describeReturnsAgent(mockClient)
			},
		},
		{
			name:             "partial fleet result with instances and errors succeeds",
			instanceTypes:    []string{"t4g.medium", "c6g.large"},
			launchTemplateID: "lt-abc",
			subnets:          []string{"subnet-a"},
			useSpotInstances: true,
			setupMocks: func(mockClient *mocks.MockEC2Client) {
				mockClient.On("CreateFleet", mock.Anything, mock.Anything).Return(&ec2.CreateFleetOutput{
					Instances: []ec2_types.CreateFleetInstance{{InstanceIds: []string{"i-9"}}},
					Errors: []ec2_types.CreateFleetError{{
						ErrorCode:    aws.String("InsufficientInstanceCapacity"),
						ErrorMessage: aws.String("no capacity for t4g.medium"),
					}},
				}, nil).Once()
				describeReturnsAgent(mockClient)
			},
		},
		{
			name:             "errors-only fleet result returns error with aws code",
			instanceTypes:    []string{"t4g.medium", "c6g.large"},
			launchTemplateID: "lt-abc",
			subnets:          []string{"subnet-a"},
			useSpotInstances: true,
			setupMocks: func(mockClient *mocks.MockEC2Client) {
				mockClient.On("CreateFleet", mock.Anything, mock.Anything).Return(&ec2.CreateFleetOutput{
					Errors: []ec2_types.CreateFleetError{{
						ErrorCode:    aws.String("InsufficientInstanceCapacity"),
						ErrorMessage: aws.String("no Spot capacity available"),
					}},
				}, nil).Once()
			},
			expectedError: "InsufficientInstanceCapacity",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := mocks.NewMockEC2Client(t)
			tt.setupMocks(mockClient)

			p := &provider{
				name:             "aws",
				config:           &config.Config{PoolID: "1"},
				instanceTypes:    tt.instanceTypes,
				launchTemplateID: tt.launchTemplateID,
				subnets:          tt.subnets,
				useSpotInstances: tt.useSpotInstances,
				sshKeyName:       tt.sshKeyName,
				client:           mockClient,
			}

			agent := &woodpecker.Agent{Name: agentName}
			err := p.DeployAgent(t.Context(), agent)

			if tt.expectedError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedError)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
