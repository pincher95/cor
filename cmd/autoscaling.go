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
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
)

// autoscalingCmd represents the autoscaling command
var autoscalingCmd = &cobra.Command{
	Use:   "autoscaling",
	Short: "Delete orphaned AWS Auto Scaling Groups",
	Long:  `Find and optionally delete unused Auto Scaling Groups (ASG) that have no active instances attached`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "force", Type: "bool"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{ASG: autoscaling.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeAutoscaling)
	},
}

func (b *AWSCommand) executeAutoscaling(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByName := (*extras)["filter-by-name"].(string)
	force := (*extras)["force"].(bool)

	collectDeletes := globals.Delete
	deleteNames := make([]string, 0)

	paginator := autoscaling.NewDescribeAutoScalingGroupsPaginator(b.AWSClient.ASG, &autoscaling.DescribeAutoScalingGroupsInput{})

	stream := printer.NewStreamTable(b.Output, true, []string{"AutoScalingGroup Name", "Min", "Desired", "Max", "Instances", "LBs", "TargetGroups"})
	stream.SetSort(globals.SortBy, globals.SortDesc)
	defer stream.Close()

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			b.Logger.LogError("failed to describe autoscaling groups", err, nil, false)
			return err
		}

		for _, asg := range page.AutoScalingGroups {
			name := aws.ToString(asg.AutoScalingGroupName)
			if filterByName != "" && !strings.Contains(name, filterByName) {
				continue
			}

			instanceCount := 0
			if asg.Instances != nil {
				instanceCount = len(asg.Instances)
			}
			lbCount := 0
			if asg.LoadBalancerNames != nil {
				lbCount = len(asg.LoadBalancerNames)
			}
			tgCount := 0
			if asg.TargetGroupARNs != nil {
				tgCount = len(asg.TargetGroupARNs)
			}

			desired := aws.ToInt32(asg.DesiredCapacity)
			min := aws.ToInt32(asg.MinSize)
			max := aws.ToInt32(asg.MaxSize)

			// Strict orphan definition (safer):
			// - no instances
			// - desired==0 and min==0
			// - not attached to any LB or TG
			if instanceCount != 0 {
				continue
			}
			if desired != 0 || min != 0 {
				continue
			}
			if lbCount != 0 || tgCount != 0 {
				continue
			}

			stream.WriteRow(name, min, desired, max, instanceCount, lbCount, tgCount)

			if collectDeletes {
				deleteNames = append(deleteNames, name)
			}
		}
	}

	if collectDeletes {
		if len(deleteNames) == 0 {
			return nil
		}
		confirm, err := confirmDelete(b.Prompter, b.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, name := range deleteNames {
			b.Logger.LogInfo("Deleting AutoScalingGroup", map[string]any{"AutoScalingGroupName": name, "ForceDelete": force})
			if _, err := b.AWSClient.ASG.DeleteAutoScalingGroup(ctx, &autoscaling.DeleteAutoScalingGroupInput{
				AutoScalingGroupName: aws.String(name),
				ForceDelete:          aws.Bool(force),
			}); err != nil {
				b.Logger.LogError("Error deleting autoscaling group", err, map[string]any{"AutoScalingGroupName": name}, false)
				return err
			}
		}
	}

	return nil
}

func init() {
	autoscalingCmd.Flags().String("filter-by-name", "", "Filter by Auto Scaling Group name (substring match).")
	autoscalingCmd.Flags().Bool("force", false, "Force delete ASG (use with caution).")
}
