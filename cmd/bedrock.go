/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanBedrock struct {
	name        string
	pmArn       string
	modelID     string
	modelUnits  int32
	status      string
	commitment  string
	created     string
	invocations int64
	reason      string
}

var bedrockCmd = &cobra.Command{
	Use:   "bedrock",
	Short: "Find idle Bedrock provisioned throughput model units",
	Long: `Lists Bedrock Provisioned Throughput allocations with zero Invocations
over the last --max-idle-days days. PT bills hourly per model unit (~$5–$80/hr
depending on model), so an idle PT can easily eat $4–60k per month.

Custom imported models are out of scope for this command — they have a
different billing model and are listed separately on the Bedrock console.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "max-idle-days", Type: "int"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{
					Bedrock:    bedrock.NewFromConfig(*cfg),
					CloudWatch: cloudwatch.NewFromConfig(*cfg),
				}
			},
		}, (*AWSCommand).executeBedrock)
	},
}

func init() {
	bedrockCmd.Flags().String("filter-by-name", "", "Filter by provisioned model name (substring match).")
	bedrockCmd.Flags().Int("max-idle-days", 7, "Flag provisioned throughput with zero invocations over this many days. 0 disables.")
}

func (a *AWSCommand) executeBedrock(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByName := normalizeFilterValue((*extras)["filter-by-name"].(string))
	maxIdleDays := (*extras)["max-idle-days"].(int)

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[bedrocktypes.ProvisionedModelSummary, orphanBedrock]{
		Headers:       []string{"Name", "Provisioned ARN", "Model", "MUs", "Status", "Commitment", "Created", fmt.Sprintf("Invocations (%dd)", maxIdleDays), "Reason"},
		ResourceLabel: "Bedrock PT",
		List: func(ctx context.Context, emit func(bedrocktypes.ProvisionedModelSummary) error) error {
			p := bedrock.NewListProvisionedModelThroughputsPaginator(a.AWSClient.Bedrock, &bedrock.ListProvisionedModelThroughputsInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, pm := range page.ProvisionedModelSummaries {
					if err := emit(pm); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, pm bedrocktypes.ProvisionedModelSummary) (*orphanBedrock, error) {
			name := aws.ToString(pm.ProvisionedModelName)
			if !matchesFilterValue(name, filterByName) {
				return nil, nil
			}
			// Only InService PTs accrue per-hour cost worth flagging.
			// Creating/Updating are transient; Failed already shows zero traffic
			// for an obvious reason.
			if pm.Status != bedrocktypes.ProvisionedModelStatusInService {
				return nil, nil
			}
			pmArn := aws.ToString(pm.ProvisionedModelArn)
			modelArn := aws.ToString(pm.ModelArn)
			modelID := modelArn[strings.LastIndex(modelArn, "/")+1:]

			created := ""
			if pm.CreationTime != nil {
				created = pm.CreationTime.UTC().Format("2006-01-02")
			}

			// Don't judge a PT that hasn't existed for the full window — its
			// zero-invocation reading is data-incomplete, not data-zero.
			matureEnough := pm.CreationTime != nil &&
				time.Since(*pm.CreationTime) >= time.Duration(maxIdleDays)*24*time.Hour
			var invocations int64
			if maxIdleDays > 0 && matureEnough {
				invocations, _ = a.bedrockInvocations(ctx, pmArn, maxIdleDays)
			}

			reason := ""
			if maxIdleDays > 0 && matureEnough && invocations == 0 {
				reason = fmt.Sprintf("Zero invocations in %d days", maxIdleDays)
			}
			if reason == "" {
				return nil, nil
			}

			return &orphanBedrock{
				name:        name,
				pmArn:       pmArn,
				modelID:     modelID,
				modelUnits:  aws.ToInt32(pm.ModelUnits),
				status:      string(pm.Status),
				commitment:  string(pm.CommitmentDuration),
				created:     created,
				invocations: invocations,
				reason:      reason,
			}, nil
		},
		ToRow: func(r orphanBedrock) []any {
			commitment := r.commitment
			if commitment == "" {
				commitment = "no-commit"
			}
			return []any{r.name, r.pmArn, r.modelID, r.modelUnits, r.status, commitment, r.created, r.invocations, r.reason}
		},
		MonthlyCost: func(r orphanBedrock) cost.USD {
			perMU := a.Pricing.BedrockProvisionedMUHour(r.modelID)
			if perMU == 0 {
				return 0
			}
			return perMU * cost.USD(r.modelUnits) * cost.USD(cost.HoursPerMonth)
		},
		DedupKey: func(r orphanBedrock) string { return r.pmArn },
		Delete: func(ctx context.Context, r orphanBedrock) error {
			a.Logger.LogInfo("Deleting Bedrock provisioned throughput", map[string]any{"name": r.name, "arn": r.pmArn})
			_, err := a.AWSClient.Bedrock.DeleteProvisionedModelThroughput(ctx, &bedrock.DeleteProvisionedModelThroughputInput{
				ProvisionedModelId: aws.String(r.pmArn),
			})
			return err
		},
		DeleteConcurrency: 1, // PT delete is rate-limited; one at a time is plenty
	})
}

// bedrockInvocations returns the sum of AWS/Bedrock Invocations for the
// given provisioned-model ARN over the last `days`. Dimension is
// "ProvisionedModelArn". Missing metrics return 0 with no error.
func (a *AWSCommand) bedrockInvocations(ctx context.Context, pmArn string, days int) (int64, error) {
	if a.AWSClient.CloudWatch == nil || pmArn == "" || days <= 0 {
		return 0, nil
	}
	end := time.Now()
	start := end.Add(-time.Duration(days) * 24 * time.Hour)
	out, err := a.AWSClient.CloudWatch.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/Bedrock"),
		MetricName: aws.String("Invocations"),
		Dimensions: []cwtypes.Dimension{{Name: aws.String("ProvisionedModelArn"), Value: aws.String(pmArn)}},
		StartTime:  &start,
		EndTime:    &end,
		Period:     aws.Int32(86400),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticSum},
	})
	if err != nil {
		return 0, err
	}
	var total int64
	for _, dp := range out.Datapoints {
		if dp.Sum != nil {
			total += int64(*dp.Sum)
		}
	}
	return total, nil
}
