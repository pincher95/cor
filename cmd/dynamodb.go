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
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

type orphanDynamoDBTable struct {
	TableName         string
	BillingMode       string
	TableStatus       string
	ItemCount         int64
	TableSize         int64
	ReadCapacity      int64
	WriteCapacity     int64
	DaysSinceActivity int64
	Reason            string
}

// dynamodbCmd represents the dynamodb command
var dynamodbCmd = &cobra.Command{
	Use:   "dynamodb",
	Short: "List orphaned DynamoDB tables",
	Long: `Finds DynamoDB tables that are potentially orphaned based on:
- Zero read/write capacity consumed in the last N days (default: 30)
- Empty tables (zero items)
- Provisioned mode with consistent zero usage

Orphaned DynamoDB tables can incur costs:
- Provisioned mode: $0.00065/hour per WCU, $0.00013/hour per RCU
- Storage: $0.25/GB-month
- On-demand is typically better for unused tables`,
	RunE: func(cmd *cobra.Command, args []string) error {
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout
		ctx := cmd.Context()
		logger := logging.NewLogger()

		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		additionalFlags := []flags.Flag{
			{Name: "days-no-activity", Type: "int"},
		}

		flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
		if err != nil {
			logger.LogError("Error getting flags", err, nil, true)
			return err
		}

		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
			Profile:    aws.String((*flagValues)["profile"].(string)),
			Region:     aws.String((*flagValues)["region"].(string)),
		}
		cfg, err := handlers.NewConfig(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			logger.LogError("Failed loading AWS client config", err, nil, true)
			return err
		}

		dynamodbClient := dynamodb.NewFromConfig(*cfg)
		cwClient := cloudwatch.NewFromConfig(*cfg)

		awsClient := &handlers.AWSClientImpl{}
		awsClient.DynamoDB = dynamodbClient
		awsClient.CloudWatch = cwClient

		return runDynamoDBCmd(ctx, &prompterClient, output, awsClient, flagValues, logger)
	},
}

func init() {
	dynamodbCmd.Flags().Int("days-no-activity", 30, "Consider tables orphaned if no activity for this many days")
}

func runDynamoDBCmd(ctx context.Context, prompter *prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any, logger *logging.Logger) error {
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logger,
		Prompter:  *prompter,
		Output:    output,
	}

	return command.executeDynamoDB(ctx, flagValues)
}

func (a *AWSCommand) executeDynamoDB(ctx context.Context, flagValues *map[string]any) error {
	rootCtx := ctx
	collectDeletes := (*flagValues)["delete"].(bool)
	daysNoActivity := int64((*flagValues)["days-no-activity"].(int))

	tableNameChan := make(chan string, 50)
	resultsChan := make(chan table.Row, 50)
	orphanTables := []orphanDynamoDBTable{}

	g, egCtx := errgroup.WithContext(ctx)

	// Goroutine to list all DynamoDB tables
	g.Go(func() error {
		defer close(tableNameChan)
		paginator := dynamodb.NewListTablesPaginator(a.AWSClient.DynamoDB, &dynamodb.ListTablesInput{})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(egCtx)
			if err != nil {
				a.Logger.LogError("Error listing DynamoDB tables", err, nil, false)
				return err
			}
			for _, tableName := range page.TableNames {
				select {
				case tableNameChan <- tableName:
				case <-egCtx.Done():
					return egCtx.Err()
				}
			}
		}
		return nil
	})

	// Worker goroutines to check each table
	numWorkers := NumGoroutines
	for range numWorkers {
		g.Go(func() error {
			for {
				select {
				case <-egCtx.Done():
					return nil
				case tableName, ok := <-tableNameChan:
					if !ok {
						return nil
					}

					orphan, err := a.checkDynamoDBOrphan(egCtx, tableName, daysNoActivity)
					if err != nil {
						a.Logger.LogError("Error checking DynamoDB table", err, map[string]any{
							"table": tableName,
						}, false)
						continue
					}

					if orphan != nil {
						select {
						case resultsChan <- table.Row{
							orphan.TableName,
							orphan.BillingMode,
							orphan.TableStatus,
							orphan.ItemCount,
							fmt.Sprintf("%.2f GB", float64(orphan.TableSize)/(1024*1024*1024)),
							orphan.ReadCapacity,
							orphan.WriteCapacity,
							fmt.Sprintf("%d days", orphan.DaysSinceActivity),
							orphan.Reason,
						}:
						case <-egCtx.Done():
							return egCtx.Err()
						}
						orphanTables = append(orphanTables, *orphan)
					}
				}
			}
		})
	}

	// Goroutine to collect results and print
	headers := []string{"Table Name", "Billing Mode", "Status", "Items", "Size", "Read Capacity", "Write Capacity", "Days No Activity", "Reason"}

	t := printer.NewStreamTable(a.Output, false, headers)
	defer t.Close()

	go func() {
		for row := range resultsChan {
			t.WriteRow(row...)
		}
	}()

	if err := g.Wait(); err != nil {
		close(resultsChan)
		return err
	}
	close(resultsChan)
	time.Sleep(100 * time.Millisecond) // Give goroutine time to finish writing

	a.Logger.LogInfo(fmt.Sprintf("Found %d orphaned DynamoDB tables", len(orphanTables)), nil)

	if collectDeletes && len(orphanTables) > 0 {
		confirm, err := confirmDelete(a.Prompter, a.Logger)
		if err != nil || !confirm {
			return err
		}

		a.Logger.LogInfo("Deleting orphaned DynamoDB tables...", nil)
		for _, tbl := range orphanTables {
			if err := a.deleteDynamoDBTable(rootCtx, tbl.TableName); err != nil {
				a.Logger.LogError("Failed to delete DynamoDB table", err, map[string]any{
					"table": tbl.TableName,
				}, false)
				return err
			}
			a.Logger.LogInfo(fmt.Sprintf("Deleted DynamoDB table: %s", tbl.TableName), nil)
		}
	}

	return nil
}

func (a *AWSCommand) checkDynamoDBOrphan(ctx context.Context, tableName string, daysNoActivity int64) (*orphanDynamoDBTable, error) {
	// Get table details
	describeOutput, err := a.AWSClient.DynamoDB.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(tableName),
	})
	if err != nil {
		return nil, err
	}

	table := describeOutput.Table

	// Check read/write activity using CloudWatch metrics
	endTime := time.Now()
	startTime := endTime.Add(-time.Duration(daysNoActivity) * 24 * time.Hour)

	// Check consumed read capacity
	readInput := &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/DynamoDB"),
		MetricName: aws.String("ConsumedReadCapacityUnits"),
		Dimensions: []cloudwatchtypes.Dimension{
			{
				Name:  aws.String("TableName"),
				Value: aws.String(tableName),
			},
		},
		StartTime:  &startTime,
		EndTime:    &endTime,
		Period:     aws.Int32(86400), // 1 day
		Statistics: []cloudwatchtypes.Statistic{cloudwatchtypes.StatisticSum},
	}

	readOutput, err := a.AWSClient.CloudWatch.GetMetricStatistics(ctx, readInput)
	if err != nil {
		return nil, err
	}

	hasReads := false
	for _, datapoint := range readOutput.Datapoints {
		if datapoint.Sum != nil && *datapoint.Sum > 0 {
			hasReads = true
			break
		}
	}

	// Check consumed write capacity
	writeInput := &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/DynamoDB"),
		MetricName: aws.String("ConsumedWriteCapacityUnits"),
		Dimensions: []cloudwatchtypes.Dimension{
			{
				Name:  aws.String("TableName"),
				Value: aws.String(tableName),
			},
		},
		StartTime:  &startTime,
		EndTime:    &endTime,
		Period:     aws.Int32(86400),
		Statistics: []cloudwatchtypes.Statistic{cloudwatchtypes.StatisticSum},
	}

	writeOutput, err := a.AWSClient.CloudWatch.GetMetricStatistics(ctx, writeInput)
	if err != nil {
		return nil, err
	}

	hasWrites := false
	for _, datapoint := range writeOutput.Datapoints {
		if datapoint.Sum != nil && *datapoint.Sum > 0 {
			hasWrites = true
			break
		}
	}

	// Determine orphan status
	itemCount := aws.ToInt64(table.ItemCount)
	isEmpty := itemCount == 0
	noActivity := !hasReads && !hasWrites

	// Only flag as orphan if no activity
	if !noActivity && !isEmpty {
		return nil, nil
	}

	billingMode := "PAY_PER_REQUEST"
	if table.BillingModeSummary != nil && table.BillingModeSummary.BillingMode == dynamodbtypes.BillingModeProvisioned {
		billingMode = "PROVISIONED"
	}

	readCapacity := int64(0)
	writeCapacity := int64(0)
	if table.ProvisionedThroughput != nil {
		readCapacity = aws.ToInt64(table.ProvisionedThroughput.ReadCapacityUnits)
		writeCapacity = aws.ToInt64(table.ProvisionedThroughput.WriteCapacityUnits)
	}

	reason := ""
	if isEmpty && noActivity {
		reason = "Empty table with no activity"
	} else if isEmpty {
		reason = "Empty table"
	} else {
		reason = fmt.Sprintf("No activity for %d days", daysNoActivity)
	}

	return &orphanDynamoDBTable{
		TableName:         aws.ToString(table.TableName),
		BillingMode:       billingMode,
		TableStatus:       string(table.TableStatus),
		ItemCount:         itemCount,
		TableSize:         aws.ToInt64(table.TableSizeBytes),
		ReadCapacity:      readCapacity,
		WriteCapacity:     writeCapacity,
		DaysSinceActivity: daysNoActivity,
		Reason:            reason,
	}, nil
}

func (a *AWSCommand) deleteDynamoDBTable(ctx context.Context, tableName string) error {
	_, err := a.AWSClient.DynamoDB.DeleteTable(ctx, &dynamodb.DeleteTableInput{
		TableName: aws.String(tableName),
	})
	return err
}
