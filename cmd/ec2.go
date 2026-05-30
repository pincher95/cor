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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanEC2 struct {
	name         string
	id           string
	instanceType string
	state        string
	launched     string
	stoppedFor   string // human-readable; empty if running
	avgCPU       float64
	reason       string
}

var ec2Cmd = &cobra.Command{
	Use:   "ec2",
	Short: "Find stopped or idle EC2 instances",
	Long: `Flags EC2 instances that are either:
  - stopped for more than --max-stopped-days days, or
  - running but averaging less than --max-cpu-percent CPU over --max-idle-days days.

Stopped instances pay for EBS but no compute. Idle running instances pay full hourly.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "max-stopped-days", Type: "int"},
				{Name: "max-idle-days", Type: "int"},
				{Name: "max-cpu-percent", Type: "int"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{
					EC2:        ec2.NewFromConfig(*cfg),
					CloudWatch: cloudwatch.NewFromConfig(*cfg),
				}
			},
		}, (*AWSCommand).executeEC2)
	},
}

func init() {
	ec2Cmd.Flags().String("filter-by-name", "", "Filter instances by Name tag (substring match).")
	ec2Cmd.Flags().Int("max-stopped-days", 14, "Flag stopped instances older than this many days. 0 disables the stopped check.")
	ec2Cmd.Flags().Int("max-idle-days", 14, "Flag running instances whose CPU has averaged below --max-cpu-percent for this many days. 0 disables the idle check.")
	ec2Cmd.Flags().Int("max-cpu-percent", 5, "CPU% threshold for the idle-running check (Average over the window).")
}

func (a *AWSCommand) executeEC2(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByName := normalizeFilterValue((*extras)["filter-by-name"].(string))
	maxStoppedDays := (*extras)["max-stopped-days"].(int)
	maxIdleDays := (*extras)["max-idle-days"].(int)
	maxCPUPercent := (*extras)["max-cpu-percent"].(int)

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[ec2types.Instance, orphanEC2]{
		Headers:       []string{"Name", "Instance ID", "Type", "State", "Launched", "Stopped For", "Avg CPU%", "Reason"},
		ResourceLabel: "EC2 instances",
		List: func(ctx context.Context, emit func(ec2types.Instance) error) error {
			p := ec2.NewDescribeInstancesPaginator(a.AWSClient.EC2, &ec2.DescribeInstancesInput{
				Filters: []ec2types.Filter{{
					Name:   aws.String("instance-state-name"),
					Values: []string{"running", "stopped"},
				}},
			})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, res := range page.Reservations {
					for _, inst := range res.Instances {
						if err := emit(inst); err != nil {
							return err
						}
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, inst ec2types.Instance) (*orphanEC2, error) {
			name := ec2NameTag(inst.Tags)
			if !matchesFilterValue(name, filterByName) {
				return nil, nil
			}
			id := aws.ToString(inst.InstanceId)
			state := string(inst.State.Name)
			launched := ""
			if inst.LaunchTime != nil {
				launched = inst.LaunchTime.UTC().Format("2006-01-02")
			}

			reason := ""
			stoppedFor := ""
			var avgCPU float64

			switch state {
			case "stopped":
				if maxStoppedDays > 0 {
					// StateTransitionReason carries the timestamp for stops,
					// but it's a free-form string. Fall back to LaunchTime as
					// the conservative lower bound on stopped-age.
					ageFrom := inst.LaunchTime
					if t := parseStateTransitionTime(aws.ToString(inst.StateTransitionReason)); !t.IsZero() {
						ageFrom = &t
					}
					if ageFrom != nil {
						age := time.Since(*ageFrom)
						if age >= time.Duration(maxStoppedDays)*24*time.Hour {
							stoppedFor = humanizeDuration(age)
							reason = fmt.Sprintf("Stopped for %s", stoppedFor)
						}
					}
				}
			case "running":
				if maxIdleDays > 0 && inst.LaunchTime != nil &&
					time.Since(*inst.LaunchTime) >= time.Duration(maxIdleDays)*24*time.Hour {
					avgCPU, _ = a.ec2AvgCPU(ctx, id, maxIdleDays)
					if avgCPU < float64(maxCPUPercent) {
						reason = fmt.Sprintf("Avg CPU %.1f%% over %d days", avgCPU, maxIdleDays)
					}
				}
			}

			if reason == "" {
				return nil, nil
			}
			if name == "" {
				name = "-"
			}
			return &orphanEC2{
				name:         name,
				id:           id,
				instanceType: string(inst.InstanceType),
				state:        state,
				launched:     launched,
				stoppedFor:   stoppedFor,
				avgCPU:       avgCPU,
				reason:       reason,
			}, nil
		},
		ToRow: func(r orphanEC2) []any {
			cpuCell := any("—")
			if r.state == "running" {
				cpuCell = fmt.Sprintf("%.1f", r.avgCPU)
			}
			return []any{r.name, r.id, r.instanceType, r.state, r.launched, r.stoppedFor, cpuCell, r.reason}
		},
		MonthlyCost: func(r orphanEC2) cost.USD {
			// Stopped instances pay only for their EBS volumes — surfaced by
			// the `volumes` command. Avoid double-counting here.
			if r.state != "running" {
				return 0
			}
			return a.Pricing.EC2InstanceMonth(r.instanceType)
		},
		DedupKey: func(r orphanEC2) string { return r.id },
		Delete: func(ctx context.Context, r orphanEC2) error {
			a.Logger.LogInfo("Terminating EC2 instance", map[string]any{"id": r.id, "reason": r.reason})
			_, err := a.AWSClient.EC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
				InstanceIds: []string{r.id},
			})
			return err
		},
		DeleteConcurrency: 5,
	})
}

// ec2AvgCPU returns the average CPU% over the last `days` from CloudWatch
// AWS/EC2 CPUUtilization. Returns 0 with no error if metrics are missing.
func (a *AWSCommand) ec2AvgCPU(ctx context.Context, instanceID string, days int) (float64, error) {
	if a.AWSClient.CloudWatch == nil || instanceID == "" || days <= 0 {
		return 0, nil
	}
	end := time.Now()
	start := end.Add(-time.Duration(days) * 24 * time.Hour)
	out, err := a.AWSClient.CloudWatch.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/EC2"),
		MetricName: aws.String("CPUUtilization"),
		Dimensions: []cwtypes.Dimension{{Name: aws.String("InstanceId"), Value: aws.String(instanceID)}},
		StartTime:  &start,
		EndTime:    &end,
		Period:     aws.Int32(86400),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticAverage},
	})
	if err != nil || len(out.Datapoints) == 0 {
		return 0, err
	}
	var sum float64
	for _, dp := range out.Datapoints {
		if dp.Average != nil {
			sum += *dp.Average
		}
	}
	return sum / float64(len(out.Datapoints)), nil
}

// parseStateTransitionTime extracts the ISO timestamp from EC2's
// StateTransitionReason, formatted like "User initiated (2025-01-15 14:32:11 GMT)".
// Returns zero time if it can't parse.
func parseStateTransitionTime(reason string) time.Time {
	open := -1
	for i := len(reason) - 1; i >= 0; i-- {
		if reason[i] == '(' {
			open = i
			break
		}
	}
	if open < 0 || open+20 > len(reason) {
		return time.Time{}
	}
	// Expect "YYYY-MM-DD HH:MM:SS GMT" inside the parens.
	candidate := reason[open+1:]
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 GMT"} {
		if t, err := time.Parse(layout, firstN(candidate, 23)); err == nil {
			return t
		}
	}
	return time.Time{}
}

func firstN(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func humanizeDuration(d time.Duration) string {
	days := int(d / (24 * time.Hour))
	if days >= 30 {
		return fmt.Sprintf("%dmo", days/30)
	}
	return fmt.Sprintf("%dd", days)
}
