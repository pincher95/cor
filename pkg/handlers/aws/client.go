package handlers

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type AWSClient interface {
	EC2Client
	ELBV2Client
}

// EC2Client is the interface for the ec2 client
type EC2Client interface {
	DescribeVolumes(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	DeleteVolume(ctx context.Context, pamams *ec2.DeleteVolumeInput, optFns ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error)
}

// ELBV2Client is an interface that defines the methods used from the AWS SDK.
type ELBV2Client interface {
	DescribeLoadBalancers(ctx context.Context, params *elasticloadbalancingv2.DescribeLoadBalancersInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancersOutput, error)
	DescribeTargetGroups(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetGroupsInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetGroupsOutput, error)
	DescribeTargetHealth(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetHealthInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetHealthOutput, error)
	DescribeListeners(ctx context.Context, params *elasticloadbalancingv2.DescribeListenersInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeListenersOutput, error)
	DeleteListener(ctx context.Context, params *elasticloadbalancingv2.DeleteListenerInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteListenerOutput, error)
	DeleteTargetGroup(ctx context.Context, params *elasticloadbalancingv2.DeleteTargetGroupInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteTargetGroupOutput, error)
	DeleteLoadBalancer(ctx context.Context, params *elasticloadbalancingv2.DeleteLoadBalancerInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteLoadBalancerOutput, error)
}

type AWSClientImpl struct {
	EC2 *ec2.Client
	ELB *elasticloadbalancingv2.Client
	STS *sts.Client
}

func (c *AWSClientImpl) DescribeVolumes(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	return c.EC2.DescribeVolumes(ctx, params, optFns...)
}

func (c *AWSClientImpl) DeleteVolume(ctx context.Context, params *ec2.DeleteVolumeInput, optFns ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	return c.EC2.DeleteVolume(ctx, params, optFns...)
}

// Similarly, implement ELB methods:
func (c *AWSClientImpl) DeleteListener(ctx context.Context, params *elasticloadbalancingv2.DeleteListenerInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteListenerOutput, error) {
	return c.ELB.DeleteListener(ctx, params, optFns...)
}

func (c *AWSClientImpl) DeleteTargetGroup(ctx context.Context, params *elasticloadbalancingv2.DeleteTargetGroupInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteTargetGroupOutput, error) {
	return c.ELB.DeleteTargetGroup(ctx, params, optFns...)
}

func (c *AWSClientImpl) DeleteLoadBalancer(ctx context.Context, params *elasticloadbalancingv2.DeleteLoadBalancerInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteLoadBalancerOutput, error) {
	return c.ELB.DeleteLoadBalancer(ctx, params, optFns...)
}

func (c *AWSClientImpl) DescribeLoadBalancers(ctx context.Context, params *elasticloadbalancingv2.DescribeLoadBalancersInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancersOutput, error) {
	return c.ELB.DescribeLoadBalancers(ctx, params, optFns...)
}

func (c *AWSClientImpl) DescribeTargetGroups(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetGroupsInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetGroupsOutput, error) {
	return c.ELB.DescribeTargetGroups(ctx, params, optFns...)
}

func (c *AWSClientImpl) DescribeTargetHealth(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetHealthInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetHealthOutput, error) {
	return c.ELB.DescribeTargetHealth(ctx, params, optFns...)
}

func (c *AWSClientImpl) GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return c.STS.GetCallerIdentity(ctx, params, optFns...)
}

// CloudConfig is the configuration for the AWS client
type CloudConfig struct {
	AuthMethod *string
	Profile    *string
	Region     *string
}

// NewConfig initializes AWS Client config
func NewConfig(authMethod string, profile string, region string, timezone string, humanize bool, debug bool) (*aws.Config, error) {
	ctx := context.Background()

	switch authMethod {
	// case "IAM_ARN":
	// 	return authenticateIAMARN(ctx, region)
	case "AWS_CREDENTIALS_FILE":
		return authenticateAWSCredentialsFile(ctx, region, profile)
	case "ENV_SECRET":
		return authenticateEnvSecret(ctx, region)
	default:
		return nil, fmt.Errorf("Unsupported authentication method")
	}

	// stsClient := sts.NewFromConfig(*cfg)

	// _ = aws.NewCredentialsCache(stscreds.NewWebIdentityRoleProvider(
	// 	stsClient,
	// 	"roleARN",
	// 	stscreds.IdentityTokenFile("tokefile"),
	// 	func(o *stscreds.WebIdentityRoleOptions) {
	// 		o.RoleSessionName = "session"
	// 	},
	// ))
	// return
}

// NewConfig initializes AWS Client config
func NewConfigV2(ctx context.Context, cloudConfig CloudConfig, timezone string, humanize bool, debug bool) (*aws.Config, error) {
	switch *cloudConfig.AuthMethod {
	// case "IAM_ARN":
	// 	return authenticateIAMARN(ctx, region)
	case "AWS_CREDENTIALS_FILE":
		return authenticateAWSCredentialsFile(ctx, *cloudConfig.Region, *cloudConfig.Profile)
	case "ENV_SECRET":
		return authenticateEnvSecret(ctx, *cloudConfig.Region)
	default:
		return nil, fmt.Errorf("Unsupported authentication method")
	}

	// stsClient := sts.NewFromConfig(*cfg)

	// _ = aws.NewCredentialsCache(stscreds.NewWebIdentityRoleProvider(
	// 	stsClient,
	// 	"roleARN",
	// 	stscreds.IdentityTokenFile("tokefile"),
	// 	func(o *stscreds.WebIdentityRoleOptions) {
	// 		o.RoleSessionName = "session"
	// 	},
	// ))
	// return
}

// IAM ARN authentication
// func authenticateIAMARN(ctx context.Context) (*aws.Config, error) {
// 	cfg, err := config.LoadDefaultConfig(ctx, config.WithCredentialsProvider(credentials.NewAssumeRoleProvider(
// 		credentials.NewStaticCredentialsProvider(os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), ""),
// 		"arn:aws:iam::123456789012:role/YourRoleName",
// 	)))
// 	if err != nil {
// 		return nil, err
// 	}
// 	return &cfg, nil
// }

// AWS credential file authentication
func authenticateAWSCredentialsFile(ctx context.Context, region string, profile string) (*aws.Config, error) {
	// cfg, err := config.LoadDefaultConfig(ctx,
	// 	config.WithRegion(region),
	// 	config.WithSharedConfigProfile(profile),
	// )

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithSharedConfigProfile(profile),
		config.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				// Makes the rate limiter more permissive in general. These values are
				// arbitrary for demonstration and may not suit your specific
				// application's needs.
				o.RateLimiter = ratelimit.NewTokenRateLimit(1000)
				// o.RetryCost = 1
				// o.RetryTimeoutCost = 3
				// o.NoRetryIncrement = 10
				o.MaxAttempts = 20
				o.MaxBackoff = 300 * time.Millisecond

			})
		}),
	)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Environment variable authentication
func authenticateEnvSecret(ctx context.Context, region string) (*aws.Config, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), "")),
		config.WithRegion(region),
	)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Confirmation asks user for confirmation.
// "y" and "Y" returns true and others are false.
// func (client *Client) Confirmation(message string) (bool, error) {
// 	fmt.Fprintf(client.stdout, "%s [y/n]: ", message)

// 	reader := bufio.NewReader(client.stdin)
// 	input, err := reader.ReadString('\n')
// 	if err != nil {
// 		return false, errors.Wrap(err, "ReadString failed:")
// 	}

// 	normalized := strings.ToLower(strings.TrimSpace(input))
// 	return normalized == "y", nil
// }
