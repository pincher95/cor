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
)

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
