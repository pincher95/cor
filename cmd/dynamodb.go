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
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
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
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "days-no-activity", Type: "int"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{
					DynamoDB:   dynamodb.NewFromConfig(*cfg),
					CloudWatch: cloudwatch.NewFromConfig(*cfg),
				}
			},
		}, (*AWSCommand).executeDynamoDB)
	},
}

func init() {
	dynamodbCmd.Flags().Int("days-no-activity", 30, "Consider tables orphaned if no activity for this many days")
}

func (a *AWSCommand) executeDynamoDB(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	daysNoActivity := int64((*extras)["days-no-activity"].(int))

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[string, orphanDynamoDBTable]{
		Headers:       []string{"Table Name", "Billing Mode", "Status", "Items", "Size", "Read Capacity", "Write Capacity", "Days No Activity", "Reason"},
		ResourceLabel: "DynamoDB tables",
		HideIndex:     true,
		List: func(ctx context.Context, emit func(string) error) error {
			p := dynamodb.NewListTablesPaginator(a.AWSClient.DynamoDB, &dynamodb.ListTablesInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, name := range page.TableNames {
					if err := emit(name); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, tableName string) (*orphanDynamoDBTable, error) {
			orphan, err := a.checkDynamoDBOrphan(ctx, tableName, daysNoActivity)
			if err != nil {
				a.Logger.LogError("Error checking DynamoDB table", err, map[string]any{
					"table": tableName,
				})
				return nil, nil
			}
			return orphan, nil
		},
		ToRow: func(r orphanDynamoDBTable) []any {
			return []any{
				r.TableName,
				r.BillingMode,
				r.TableStatus,
				r.ItemCount,
				fmt.Sprintf("%.2f GB", float64(r.TableSize)/(1024*1024*1024)),
				r.ReadCapacity,
				r.WriteCapacity,
				fmt.Sprintf("%d days", r.DaysSinceActivity),
				r.Reason,
			}
		},
		Delete: func(ctx context.Context, r orphanDynamoDBTable) error {
			a.Logger.LogInfo("Deleting DynamoDB table", map[string]any{"TableName": r.TableName})
			if err := a.deleteDynamoDBTable(ctx, r.TableName); err != nil {
				a.Logger.LogError("Failed to delete DynamoDB table", err, map[string]any{"TableName": r.TableName})
				return err
			}
			return nil
		},
		MonthlyCost: func(r orphanDynamoDBTable) cost.USD {
			gb := float64(r.TableSize) / (1024 * 1024 * 1024)
			storage := cost.USD(gb) * a.Pricing.DynamoDBStorageGB()
			if r.BillingMode != "PROVISIONED" {
				return storage
			}
			wcu := cost.USD(r.WriteCapacity) * a.Pricing.DynamoDBWCUMonth()
			rcu := cost.USD(r.ReadCapacity) * a.Pricing.DynamoDBRCUMonth()
			return storage + wcu + rcu
		},
	})
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

	window := time.Duration(daysNoActivity) * 24 * time.Hour
	dim := []cloudwatchtypes.Dimension{{Name: aws.String("TableName"), Value: aws.String(tableName)}}
	idleReads, err := a.IsIdle(ctx, IdleSpec{Namespace: "AWS/DynamoDB", MetricName: "ConsumedReadCapacityUnits", Dimensions: dim, Window: window})
	if err != nil {
		return nil, err
	}
	idleWrites, err := a.IsIdle(ctx, IdleSpec{Namespace: "AWS/DynamoDB", MetricName: "ConsumedWriteCapacityUnits", Dimensions: dim, Window: window})
	if err != nil {
		return nil, err
	}

	itemCount := aws.ToInt64(table.ItemCount)
	isEmpty := itemCount == 0
	noActivity := idleReads && idleWrites

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
