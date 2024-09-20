package cmd

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockELBV2Client is a mock implementation of the ELBV2Client interface using testify.
type MockELBV2Client struct {
	mock.Mock
}

func (m *MockELBV2Client) DescribeLoadBalancers(ctx context.Context, params *elasticloadbalancingv2.DescribeLoadBalancersInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancersOutput, error) {
	args := m.Called(ctx, params)
	return args.Get(0).(*elasticloadbalancingv2.DescribeLoadBalancersOutput), args.Error(1)
}

func (m *MockELBV2Client) DescribeTargetGroups(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetGroupsInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetGroupsOutput, error) {
	args := m.Called(ctx, params)
	return args.Get(0).(*elasticloadbalancingv2.DescribeTargetGroupsOutput), args.Error(1)
}

func (m *MockELBV2Client) DescribeTargetHealth(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetHealthInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetHealthOutput, error) {
	args := m.Called(ctx, params)
	return args.Get(0).(*elasticloadbalancingv2.DescribeTargetHealthOutput), args.Error(1)
}

func (m *MockELBV2Client) DescribeListeners(ctx context.Context, params *elasticloadbalancingv2.DescribeListenersInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeListenersOutput, error) {
	args := m.Called(ctx, params)
	return args.Get(0).(*elasticloadbalancingv2.DescribeListenersOutput), args.Error(1)
}

func (m *MockELBV2Client) DeleteListener(ctx context.Context, params *elasticloadbalancingv2.DeleteListenerInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteListenerOutput, error) {
	args := m.Called(ctx, params)
	return args.Get(0).(*elasticloadbalancingv2.DeleteListenerOutput), args.Error(1)
}

func (m *MockELBV2Client) DeleteTargetGroup(ctx context.Context, params *elasticloadbalancingv2.DeleteTargetGroupInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteTargetGroupOutput, error) {
	args := m.Called(ctx, params)
	return args.Get(0).(*elasticloadbalancingv2.DeleteTargetGroupOutput), args.Error(1)
}

func (m *MockELBV2Client) DeleteLoadBalancer(ctx context.Context, params *elasticloadbalancingv2.DeleteLoadBalancerInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteLoadBalancerOutput, error) {
	args := m.Called(ctx, params)
	return args.Get(0).(*elasticloadbalancingv2.DeleteLoadBalancerOutput), args.Error(1)
}

// MockPrompter is a mock implementation of the Prompter interface using testify.
type MockPrompter struct {
	mock.Mock
}

func (m *MockPrompter) Confirm(prompt string) (*bool, error) {
	args := m.Called(prompt)
	result := args.Bool(0)
	return &result, args.Error(1)
}

func TestHandleLoadBalancerV2(t *testing.T) {
	ctx := context.Background()

	// Define test cases
	testCases := []struct {
		name          string
		elb           types.LoadBalancer
		filterByName  string
		mockSetup     func(m *MockELBV2Client)
		expectedRow   *table.Row
		expectedError error
	}{
		{
			name: "Load balancer with empty target groups",
			elb: types.LoadBalancer{
				LoadBalancerName: aws.String("test-lb"),
				LoadBalancerArn:  aws.String("arn:aws:elasticloadbalancing:region:account-id:loadbalancer/app/test-lb"),
				VpcId:            aws.String("vpc-12345678"),
			},
			filterByName: "test-lb",
			mockSetup: func(m *MockELBV2Client) {
				// Mock DescribeTargetGroups
				m.On("DescribeTargetGroups", ctx, &elasticloadbalancingv2.DescribeTargetGroupsInput{
					LoadBalancerArn: aws.String("arn:aws:elasticloadbalancing:region:account-id:loadbalancer/app/test-lb"),
				}).Return(&elasticloadbalancingv2.DescribeTargetGroupsOutput{
					TargetGroups: []types.TargetGroup{
						{
							TargetGroupName: aws.String("tg-1"),
							TargetGroupArn:  aws.String("arn:aws:elasticloadbalancing:region:account-id:targetgroup/tg-1"),
						},
					},
				}, nil)

				// Mock DescribeTargetHealth for empty target group
				m.On("DescribeTargetHealth", ctx, &elasticloadbalancingv2.DescribeTargetHealthInput{
					TargetGroupArn: aws.String("arn:aws:elasticloadbalancing:region:account-id:targetgroup/tg-1"),
				}).Return(&elasticloadbalancingv2.DescribeTargetHealthOutput{
					TargetHealthDescriptions: []types.TargetHealthDescription{},
				}, nil)
			},
			expectedRow: &table.Row{
				"test-lb",
				"arn:aws:elasticloadbalancing:region:account-id:loadbalancer/app/test-lb",
				"tg-1",
				"vpc-12345678",
			},
			expectedError: nil,
		},
		{
			name: "Load balancer with target groups having targets",
			elb: types.LoadBalancer{
				LoadBalancerName: aws.String("test-lb-2"),
				LoadBalancerArn:  aws.String("arn:aws:elasticloadbalancing:region:account-id:loadbalancer/app/test-lb-2"),
				VpcId:            aws.String("vpc-87654321"),
			},
			filterByName: "test-lb-2",
			mockSetup: func(m *MockELBV2Client) {
				// Mock DescribeTargetGroups
				m.On("DescribeTargetGroups", ctx, &elasticloadbalancingv2.DescribeTargetGroupsInput{
					LoadBalancerArn: aws.String("arn:aws:elasticloadbalancing:region:account-id:loadbalancer/app/test-lb-2"),
				}).Return(&elasticloadbalancingv2.DescribeTargetGroupsOutput{
					TargetGroups: []types.TargetGroup{
						{
							TargetGroupName: aws.String("tg-2"),
							TargetGroupArn:  aws.String("arn:aws:elasticloadbalancing:region:account-id:targetgroup/tg-2"),
						},
					},
				}, nil)

				// Mock DescribeTargetHealth with targets
				m.On("DescribeTargetHealth", ctx, &elasticloadbalancingv2.DescribeTargetHealthInput{
					TargetGroupArn: aws.String("arn:aws:elasticloadbalancing:region:account-id:targetgroup/tg-2"),
				}).Return(&elasticloadbalancingv2.DescribeTargetHealthOutput{
					TargetHealthDescriptions: []types.TargetHealthDescription{
						{
							Target: &types.TargetDescription{
								Id: aws.String("i-1234567890abcdef0"),
							},
							TargetHealth: &types.TargetHealth{
								State: types.TargetHealthStateEnumHealthy,
							},
						},
					},
				}, nil)
			},
			expectedRow:   nil,
			expectedError: nil,
		},
		{
			name: "Load balancer name does not match filter",
			elb: types.LoadBalancer{
				LoadBalancerName: aws.String("other-lb"),
				LoadBalancerArn:  aws.String("arn:aws:elasticloadbalancing:region:account-id:loadbalancer/app/other-lb"),
				VpcId:            aws.String("vpc-12345678"),
			},
			filterByName:  "test-lb",
			mockSetup:     func(m *MockELBV2Client) {},
			expectedRow:   nil,
			expectedError: nil,
		},
		{
			name: "Error in DescribeTargetGroups",
			elb: types.LoadBalancer{
				LoadBalancerName: aws.String("error-lb"),
				LoadBalancerArn:  aws.String("arn:aws:elasticloadbalancing:region:account-id:loadbalancer/app/error-lb"),
				VpcId:            aws.String("vpc-12345678"),
			},
			filterByName: "error-lb",
			mockSetup: func(m *MockELBV2Client) {
				// Mock DescribeTargetGroups with error
				m.On("DescribeTargetGroups", ctx, &elasticloadbalancingv2.DescribeTargetGroupsInput{
					LoadBalancerArn: aws.String("arn:aws:elasticloadbalancing:region:account-id:loadbalancer/app/error-lb"),
				}).Return((*elasticloadbalancingv2.DescribeTargetGroupsOutput)(nil), assert.AnError)
			},
			expectedRow:   nil,
			expectedError: assert.AnError,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create a new MockELBV2Client
			mockClient := new(MockELBV2Client)

			// Setup the mock behavior
			tc.mockSetup(mockClient)

			// Instantiate elbv2Command to call the method
			elbCmd := &elbv2Command{
				client: mockClient, // Assuming client is part of this struct
			}

			// Call the function under test
			row, err := elbCmd.handleLoadBalancerV2(ctx, tc.elb, tc.filterByName)

			// Assertions
			if tc.expectedError != nil {
				assert.ErrorIs(t, err, tc.expectedError)
			} else {
				assert.NoError(t, err)
			}

			if tc.expectedRow != nil {
				assert.Equal(t, *tc.expectedRow, *row)
			} else {
				assert.Nil(t, row)
			}

			// Ensure all expectations were met
			mockClient.AssertExpectations(t)
		})
	}
}

// func TestElbv2Cmd_DeleteFlag(t *testing.T) {
// 	// Create a new command instance
// 	cmd := &cobra.Command{}
// 	cmd.Flags().String("auth-method", "", "The AWS region to use")
// 	cmd.Flags().String("region", "", "The AWS region to use")
// 	cmd.Flags().Bool("delete", false, "")
// 	cmd.Flags().String("filter-by-name", "", "")

// 	// Set flags
// 	if err := cmd.Flags().Set("region", "us-east-1"); err != nil {
// 		t.Fatal(err)
// 	}
// 	if err := cmd.Flags().Set("delete", "true"); err != nil {
// 		t.Fatal(err)
// 	}
// 	if err := cmd.Flags().Set("filter-by-name", "test-lb"); err != nil {
// 		t.Fatal(err)
// 	}

// 	flagValues := map[string]interface{}{"profile": "default", "auth-method": "AWS_CREDENTIALS_FILE", "region": "us-east-1", "delete": true, "filter-by-name": "test-lb"}

// 	// Prepare output
// 	output := &strings.Builder{}

// 	// Create a mock client
// 	mockClient := new(MockELBV2Client)

// 	// Create a mock prompter that simulates user typing 'yes'
// 	mockPrompter := new(MockPrompter)
// 	mockPrompter.On("Confirm", "Are you sure you want to proceed? (yes/no): ").Return(true, nil)

// 	ctx := context.Background()

// 	// Setup expected AWS SDK calls
// 	// Mock DescribeLoadBalancers
// 	mockClient.On("DescribeLoadBalancers", ctx, mock.Anything).Return(&elasticloadbalancingv2.DescribeLoadBalancersOutput{
// 		LoadBalancers: []types.LoadBalancer{
// 			{
// 				LoadBalancerName: aws.String("test-lb"),
// 				LoadBalancerArn:  aws.String("arn:aws:elasticloadbalancing:region:account-id:loadbalancer/app/test-lb"),
// 				VpcId:            aws.String("vpc-12345678"),
// 			},
// 		},
// 	}, nil)

// 	// Mock DescribeTargetGroups
// 	mockClient.On("DescribeTargetGroups", ctx, mock.Anything).Return(&elasticloadbalancingv2.DescribeTargetGroupsOutput{
// 		TargetGroups: []types.TargetGroup{
// 			{
// 				TargetGroupName: aws.String("tg-1"),
// 				TargetGroupArn:  aws.String("arn:aws:elasticloadbalancing:region:account-id:targetgroup/tg-1"),
// 			},
// 		},
// 	}, nil)

// 	// Mock DescribeTargetHealth with no targets
// 	mockClient.On("DescribeTargetHealth", ctx, mock.Anything).Return(&elasticloadbalancingv2.DescribeTargetHealthOutput{
// 		TargetHealthDescriptions: []types.TargetHealthDescription{},
// 	}, nil)

// 	// Mock DescribeListeners
// 	mockClient.On("DescribeListeners", ctx, mock.Anything).Return(&elasticloadbalancingv2.DescribeListenersOutput{
// 		Listeners: []types.Listener{
// 			{
// 				ListenerArn: aws.String("arn:aws:elasticloadbalancing:region:account-id:listener/app/test-lb/listener-1"),
// 			},
// 		},
// 	}, nil)

// 	// Mock DeleteListener to return an empty output, simulating a successful deletion
// 	mockClient.On("DeleteListener", ctx, mock.Anything).Return(&elasticloadbalancingv2.DeleteListenerOutput{}, nil)

// 	// Mock DeleteTargetGroup to return an empty output, simulating a successful deletion
// 	mockClient.On("DeleteTargetGroup", ctx, mock.Anything).Return(&elasticloadbalancingv2.DeleteTargetGroupOutput{}, nil)

// 	// Mock DeleteLoadBalancer to return an empty output, simulating a successful deletion
// 	mockClient.On("DeleteLoadBalancer", ctx, mock.Anything).Return(&elasticloadbalancingv2.DeleteLoadBalancerOutput{}, nil)

// 	// Run the command
// 	err := runElbv2Cmd(ctx, mockPrompter, output, mockClient, flagValues)

// 	// Assertions
// 	assert.NoError(t, err)

// 	// Verify that the expected AWS methods were called
// 	mockClient.AssertExpectations(t)
// 	mockPrompter.AssertExpectations(t)

// 	// Check the output
// 	outputStr := output.String()
// 	fmt.Println(outputStr)
// 	assert.Contains(t, outputStr, "")
// 	assert.Contains(t, outputStr, "")
// 	assert.Contains(t, outputStr, "")
// 	assert.Equal(t, "expected", actual, "Expected %v but got %v", expected, actual)
// }

// func TestElbv2Cmd_DeleteFlag_UserRejects(t *testing.T) {
// 	// Create a new command instance
// 	cmd := &cobra.Command{}
// 	cmd.Flags().String("auth-method", "", "The AWS region to use")
// 	cmd.Flags().String("region", "", "The AWS region to use")
// 	cmd.Flags().Bool("delete", false, "")
// 	cmd.Flags().String("filter-by-name", "", "")

// 	// Set flags
// 	if err := cmd.Flags().Set("region", "us-east-1"); err != nil {
// 		t.Fatal(err)
// 	}
// 	if err := cmd.Flags().Set("delete", "true"); err != nil {
// 		t.Fatal(err)
// 	}
// 	if err := cmd.Flags().Set("filter-by-name", "test-lb"); err != nil {
// 		t.Fatal(err)
// 	}

// 	flagValues := map[string]interface{}{"profile": "default", "auth-method": "AWS_CREDENTIALS_FILE", "region": "us-east-1", "delete": true, "filter-by-name": "test-lb"}

// 	// Prepare output
// 	output := &strings.Builder{}

// 	// Create a mock client
// 	mockClient := new(MockELBV2Client)

// 	// Create a mock prompter that simulates user typing 'no'
// 	mockPrompter := new(MockPrompter)
// 	mockPrompter.On("Confirm", "Are you sure you want to proceed? (yes/no): ").Return(false, nil)

// 	ctx := context.Background()

// 	// Mock DescribeLoadBalancers
// 	mockClient.On("DescribeLoadBalancers", ctx, mock.Anything).Return(&elasticloadbalancingv2.DescribeLoadBalancersOutput{
// 		LoadBalancers: []types.LoadBalancer{
// 			{
// 				LoadBalancerName: aws.String("test-lb"),
// 				LoadBalancerArn:  aws.String("arn:aws:elasticloadbalancing:region:account-id:loadbalancer/app/test-lb"),
// 				VpcId:            aws.String("vpc-12345678"),
// 			},
// 		},
// 	}, nil)

// 	// Mock DescribeTargetGroups
// 	mockClient.On("DescribeTargetGroups", ctx, mock.Anything).Return(&elasticloadbalancingv2.DescribeTargetGroupsOutput{
// 		TargetGroups: []types.TargetGroup{
// 			{
// 				TargetGroupName: aws.String("tg-1"),
// 				TargetGroupArn:  aws.String("arn:aws:elasticloadbalancing:region:account-id:targetgroup/tg-1"),
// 			},
// 		},
// 	}, nil)

// 	// Mock DescribeTargetHealth
// 	mockClient.On("DescribeTargetHealth", ctx, mock.Anything).Return(&elasticloadbalancingv2.DescribeTargetHealthOutput{
// 		TargetHealthDescriptions: []types.TargetHealthDescription{},
// 	}, nil)

// 	// Run the command
// 	err := runElbv2Cmd(ctx, mockPrompter, output, mockClient, flagValues)

// 	// Assertions
// 	assert.NoError(t, err)

// 	// Verify that deletion methods were not called
// 	mockClient.AssertNotCalled(t, "DeleteListener", mock.Anything, mock.Anything)
// 	mockClient.AssertNotCalled(t, "DeleteTargetGroup", mock.Anything, mock.Anything)
// 	mockClient.AssertNotCalled(t, "DeleteLoadBalancer", mock.Anything, mock.Anything)

// 	// Verify that the prompt was called
// 	mockPrompter.AssertExpectations(t)

// 	// Check the output
// 	outputStr := output.String()
// 	assert.Contains(t, outputStr, "Aborted.")
// }
// func TestElbv2Cmd_DeleteFlag_ErrorDuringDeletion(t *testing.T) {
// 	// Create a new command instance
// 	cmd := &cobra.Command{}
// 	cmd.Flags().String("auth-method", "", "The AWS region to use")
// 	cmd.Flags().String("region", "", "The AWS region to use")
// 	cmd.Flags().Bool("delete", false, "")
// 	cmd.Flags().String("filter-by-name", "", "")

// 	// Set flags
// 	if err := cmd.Flags().Set("region", "us-east-1"); err != nil {
// 		t.Fatal(err)
// 	}
// 	if err := cmd.Flags().Set("delete", "true"); err != nil {
// 		t.Fatal(err)
// 	}
// 	if err := cmd.Flags().Set("filter-by-name", "test-lb"); err != nil {
// 		t.Fatal(err)
// 	}

// 	flagValues := map[string]interface{}{"profile": "default", "auth-method": "AWS_CREDENTIALS_FILE", "region": "us-east-1", "delete": true, "filter-by-name": "test-lb"}

// 	// Prepare output
// 	output := &strings.Builder{}

// 	// Create a mock client
// 	mockClient := new(MockELBV2Client)

// 	// Create a mock prompter that simulates user typing 'yes'
// 	mockPrompter := new(MockPrompter)
// 	mockPrompter.On("Confirm", "Are you sure you want to proceed? (yes/no): ").Return(true, nil)

// 	ctx := context.Background()

// 	// Mock DescribeLoadBalancers
// 	mockClient.On("DescribeLoadBalancers", ctx, mock.Anything).Return(&elasticloadbalancingv2.DescribeLoadBalancersOutput{
// 		LoadBalancers: []types.LoadBalancer{
// 			{
// 				LoadBalancerName: aws.String("test-lb"),
// 				LoadBalancerArn:  aws.String("arn:aws:elasticloadbalancing:region:account-id:loadbalancer/app/test-lb"),
// 				VpcId:            aws.String("vpc-12345678"),
// 			},
// 		},
// 	}, nil)

// 	// Mock DescribeTargetGroups
// 	mockClient.On("DescribeTargetGroups", ctx, mock.MatchedBy(func(input *elasticloadbalancingv2.DescribeTargetGroupsInput) bool {
// 		if input.LoadBalancerArn != nil && *input.LoadBalancerArn == "arn:aws:elasticloadbalancing:region:account-id:loadbalancer/app/test-lb" {
// 			return true
// 		}
// 		if input.Names != nil && len(input.Names) > 0 && input.Names[0] == "tg-1" {
// 			return true
// 		}
// 		return false
// 	})).Return(&elasticloadbalancingv2.DescribeTargetGroupsOutput{
// 		TargetGroups: []types.TargetGroup{
// 			{
// 				TargetGroupName: aws.String("tg-1"),
// 				TargetGroupArn:  aws.String("arn:aws:elasticloadbalancing:region:account-id:targetgroup/tg-1"),
// 			},
// 		},
// 	}, nil)

// 	// Mock DescribeTargetHealth
// 	mockClient.On("DescribeTargetHealth", ctx, mock.Anything).Return(&elasticloadbalancingv2.DescribeTargetHealthOutput{
// 		TargetHealthDescriptions: []types.TargetHealthDescription{},
// 	}, nil)

// 	// Mock DescribeListeners
// 	mockClient.On("DescribeListeners", ctx, mock.Anything).Return(&elasticloadbalancingv2.DescribeListenersOutput{
// 		Listeners: []types.Listener{
// 			{
// 				ListenerArn: aws.String("arn:aws:elasticloadbalancing:region:account-id:listener/app/test-lb/listener-1"),
// 			},
// 		},
// 	}, nil)

// 	// Mock DeleteListener to return an error
// 	mockClient.On("DeleteListener", ctx, mock.Anything).Return(nil, errors.New("delete listener error"))

// 	// Run the command
// 	err := runElbv2Cmd(ctx, mockPrompter, output, mockClient, flagValues)

// 	// Assertions
// 	assert.Error(t, err)
// 	assert.EqualError(t, err, "delete listener error")

// 	// Verify that the deletion methods were called up to the point of failure
// 	mockClient.AssertCalled(t, "DeleteListener", ctx, mock.Anything)
// 	// Since DeleteListener failed, DeleteTargetGroup and DeleteLoadBalancer should not be called
// 	mockClient.AssertNotCalled(t, "DeleteTargetGroup", mock.Anything, mock.Anything)
// 	mockClient.AssertNotCalled(t, "DeleteLoadBalancer", mock.Anything, mock.Anything)

// 	// Verify that the prompt was called
// 	mockPrompter.AssertExpectations(t)

// 	// Check the output
// 	outputStr := output.String()
// 	assert.Contains(t, outputStr, "Deleting listener")
// }
