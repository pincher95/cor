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

func (m *MockELBV2Client) DescribeTargetGroups(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetGroupsInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetGroupsOutput, error) {
	args := m.Called(ctx, params)
	return args.Get(0).(*elasticloadbalancingv2.DescribeTargetGroupsOutput), args.Error(1)
}

func (m *MockELBV2Client) DescribeTargetHealth(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetHealthInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetHealthOutput, error) {
	args := m.Called(ctx, params)
	return args.Get(0).(*elasticloadbalancingv2.DescribeTargetHealthOutput), args.Error(1)
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

			// Call the function under test
			row, err := handleLoadBalancerV2(ctx, mockClient, tc.elb, tc.filterByName)

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
