/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/opensearch"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type AWSClientImpl struct {
	EC2         *ec2.Client
	ELB         *elasticloadbalancingv2.Client
	ELBv1       *elasticloadbalancing.Client
	STS         *sts.Client
	ASG         *autoscaling.Client
	RDS         *rds.Client
	CWL         *cloudwatchlogs.Client
	EFS         *efs.Client
	ECR         *ecr.Client
	R53         *route53.Client
	Lambda      *lambda.Client
	CloudWatch  *cloudwatch.Client
	ElastiCache *elasticache.Client
	OpenSearch  *opensearch.Client
	DynamoDB    *dynamodb.Client
	S3          *s3.Client
	ECS         *ecs.Client
}

// CloudConfig is the configuration for the AWS client
type CloudConfig struct {
	AuthMethod *string
	Profile    *string
	Region     *string
}

func newRetryer() aws.Retryer {
	// Fail-fast for auth/signing errors that won't succeed on retry (e.g. clock skew,
	// expired credentials). Keep aggressive retries for transient/throttling errors.
	nonRetryableCodes := map[string]struct{}{
		"RequestExpired":              {},
		"ExpiredToken":                {},
		"ExpiredTokenException":       {},
		"InvalidClientTokenId":        {},
		"UnrecognizedClientException": {},
		"InvalidSignatureException":   {},
		"SignatureDoesNotMatch":       {},
	}

	failFastAuthErrors := retry.IsErrorRetryableFunc(func(err error) aws.Ternary {
		var v interface{ ErrorCode() string }
		if errors.As(err, &v) {
			if _, ok := nonRetryableCodes[v.ErrorCode()]; ok {
				return aws.FalseTernary
			}
		}
		return aws.UnknownTernary
	})

	return retry.NewStandard(func(o *retry.StandardOptions) {
		// Ensure our fail-fast logic is checked before the SDK's default retryables.
		o.Retryables = append([]retry.IsErrorRetryable{failFastAuthErrors}, o.Retryables...)

		// Makes the rate limiter more permissive in general. These values are
		// arbitrary for demonstration and may not suit your specific application's needs.
		o.RateLimiter = ratelimit.NewTokenRateLimit(500)
		o.RetryCost = 2         // Cost per retry
		o.RetryTimeoutCost = 4  // Additional cost for timeout retries
		o.NoRetryIncrement = 20 // Adds to retry quota for successful calls
		o.MaxAttempts = 20
		o.MaxBackoff = 3 * time.Second // Increase for better throttling tolerance
	})
}

// NewConfig initializes AWS Client config.
func NewConfig(ctx context.Context, cloudConfig CloudConfig, timezone string, humanize bool, debug bool) (*aws.Config, error) {
	switch *cloudConfig.AuthMethod {
	// case "IAM_ARN":
	// 	return authenticateIAMARN(ctx, region)
	case "AWS_CREDENTIALS_FILE":
		return authenticateAWSCredentialsFile(ctx, *cloudConfig.Region, *cloudConfig.Profile)
	case "ENV_SECRET":
		return authenticateEnvSecret(ctx, *cloudConfig.Region)
	default:
		return nil, fmt.Errorf("unsupported auth-method %q (allowed: AWS_CREDENTIALS_FILE, ENV_SECRET)", *cloudConfig.AuthMethod)
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
	// Load the default config
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithSharedConfigProfile(profile),
		config.WithRetryer(func() aws.Retryer {
			return newRetryer()
		}),
		config.WithHTTPClient(&http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 200,
				MaxConnsPerHost:     50,
			},
			Timeout: 60 * time.Second,
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
		config.WithRetryer(func() aws.Retryer { return newRetryer() }),
	)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Note: we intentionally avoid calling os.Exit or making extra network calls here.
// Callers can optionally validate credentials via STS if they want a preflight check.
