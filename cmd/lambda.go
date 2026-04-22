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
package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanLambdaFunction struct {
	Name                   string
	Runtime                string
	MemorySize             int32
	LastModified           string
	LastInvocationDays     int64
	Versions               int32
	ProvisionedConcurrency bool
	Reason                 string
}

// lambdaCmd represents the lambda command
var lambdaCmd = &cobra.Command{
	Use:   "lambda",
	Short: "List orphaned Lambda functions",
	Long: `Finds Lambda functions that are potentially orphaned based on:
- Functions not invoked in the last N days (default: 90)
- Functions with excessive old versions (configurable threshold)
- Functions with provisioned concurrency but no recent invocations

Orphaned Lambda functions can incur costs through:
- Storage for function code and layers
- Provisioned concurrency charges ($100-500/month per function)
- Retention of old function versions`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "days-since-invocation", Type: "int"},
				{Name: "min-old-versions", Type: "int"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{
					Lambda:     lambda.NewFromConfig(*cfg),
					CloudWatch: cloudwatch.NewFromConfig(*cfg),
				}
			},
		}, (*AWSCommand).executeLambda)
	},
}

func init() {
	lambdaCmd.Flags().Int("days-since-invocation", 90, "Consider functions orphaned if not invoked in this many days")
	lambdaCmd.Flags().Int("min-old-versions", 10, "Minimum number of old versions to report")
}

func (a *AWSCommand) executeLambda(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	daysSinceInvocation := int64((*extras)["days-since-invocation"].(int))
	minOldVersions := int32((*extras)["min-old-versions"].(int))

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[lambdatypes.FunctionConfiguration, orphanLambdaFunction]{
		Headers:       []string{"Function Name", "Runtime", "Memory", "Last Modified", "Days Since Invocation", "Versions", "Provisioned Concurrency", "Reason"},
		ResourceLabel: "Lambda functions",
		HideIndex:     true,
		List: func(ctx context.Context, emit func(lambdatypes.FunctionConfiguration) error) error {
			p := lambda.NewListFunctionsPaginator(a.AWSClient.Lambda, &lambda.ListFunctionsInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, fn := range page.Functions {
					if err := emit(fn); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, fn lambdatypes.FunctionConfiguration) (*orphanLambdaFunction, error) {
			orphan, err := a.checkLambdaOrphan(ctx, &fn, daysSinceInvocation, minOldVersions)
			if err != nil {
				a.Logger.LogError("Error checking Lambda function", err, map[string]any{
					"function": aws.ToString(fn.FunctionName),
				}, false)
				return nil, nil
			}
			return orphan, nil
		},
		ToRow: func(r orphanLambdaFunction) []any {
			return []any{
				r.Name,
				r.Runtime,
				fmt.Sprintf("%d MB", r.MemorySize),
				r.LastModified,
				fmt.Sprintf("%d days", r.LastInvocationDays),
				r.Versions,
				r.ProvisionedConcurrency,
				r.Reason,
			}
		},
		Delete: func(ctx context.Context, r orphanLambdaFunction) error {
			a.Logger.LogInfo("Deleting Lambda function", map[string]any{"function": r.Name})
			if err := a.deleteLambdaFunction(ctx, r.Name); err != nil {
				a.Logger.LogError("Failed to delete Lambda function", err, map[string]any{"function": r.Name}, false)
				return err
			}
			return nil
		},
	})
}

func (a *AWSCommand) checkLambdaOrphan(ctx context.Context, fn *lambdatypes.FunctionConfiguration, daysSinceInvocation int64, minOldVersions int32) (*orphanLambdaFunction, error) {
	functionName := aws.ToString(fn.FunctionName)

	// Check last invocation time using CloudWatch metrics
	endTime := time.Now()
	startTime := endTime.Add(-time.Duration(daysSinceInvocation) * 24 * time.Hour)

	metricsInput := &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/Lambda"),
		MetricName: aws.String("Invocations"),
		Dimensions: []cloudwatchtypes.Dimension{
			{
				Name:  aws.String("FunctionName"),
				Value: aws.String(functionName),
			},
		},
		StartTime:  &startTime,
		EndTime:    &endTime,
		Period:     aws.Int32(86400), // 1 day in seconds
		Statistics: []cloudwatchtypes.Statistic{cloudwatchtypes.StatisticSum},
	}

	metricsOutput, err := a.AWSClient.CloudWatch.GetMetricStatistics(ctx, metricsInput)
	if err != nil {
		return nil, err
	}

	// Check if function has any invocations in the period
	hasInvocations := false
	for _, datapoint := range metricsOutput.Datapoints {
		if datapoint.Sum != nil && *datapoint.Sum > 0 {
			hasInvocations = true
			break
		}
	}

	// Count function versions
	versionsPaginator := lambda.NewListVersionsByFunctionPaginator(a.AWSClient.Lambda, &lambda.ListVersionsByFunctionInput{
		FunctionName: fn.FunctionName,
	})

	versionCount := int32(0)
	for versionsPaginator.HasMorePages() {
		versionsPage, err := versionsPaginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		// Don't count $LATEST
		for _, v := range versionsPage.Versions {
			if aws.ToString(v.Version) != "$LATEST" {
				versionCount++
			}
		}
	}

	// Check for provisioned concurrency
	hasProvisionedConcurrency := false
	pcOutput, err := a.AWSClient.Lambda.ListProvisionedConcurrencyConfigs(ctx, &lambda.ListProvisionedConcurrencyConfigsInput{
		FunctionName: fn.FunctionName,
	})
	if err == nil && len(pcOutput.ProvisionedConcurrencyConfigs) > 0 {
		hasProvisionedConcurrency = true
	}

	// Determine if orphaned
	reasons := []string{}
	if !hasInvocations {
		reasons = append(reasons, fmt.Sprintf("No invocations in %d days", daysSinceInvocation))
	}
	if versionCount >= minOldVersions {
		reasons = append(reasons, fmt.Sprintf("%d old versions", versionCount))
	}
	if hasProvisionedConcurrency && !hasInvocations {
		reasons = append(reasons, "Provisioned concurrency with no invocations")
	}

	if len(reasons) == 0 {
		return nil, nil
	}

	return &orphanLambdaFunction{
		Name:                   functionName,
		Runtime:                string(fn.Runtime),
		MemorySize:             aws.ToInt32(fn.MemorySize),
		LastModified:           aws.ToString(fn.LastModified),
		LastInvocationDays:     daysSinceInvocation,
		Versions:               versionCount,
		ProvisionedConcurrency: hasProvisionedConcurrency,
		Reason:                 reasons[0], // Show primary reason
	}, nil
}

func (a *AWSCommand) deleteLambdaFunction(ctx context.Context, functionName string) error {
	_, err := a.AWSClient.Lambda.DeleteFunction(ctx, &lambda.DeleteFunctionInput{
		FunctionName: aws.String(functionName),
	})
	return err
}
