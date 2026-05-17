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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanAutoScalingGroup struct {
	name              string
	min, desired, max int32
	instances, lbs    int
	targetGroups      int
}

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

func init() {
	autoscalingCmd.Flags().String("filter-by-name", "", "Filter by Auto Scaling Group name (substring match).")
	autoscalingCmd.Flags().Bool("force", false, "Force delete ASG (use with caution).")
}

func (a *AWSCommand) executeAutoscaling(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByName := (*extras)["filter-by-name"].(string)
	force := (*extras)["force"].(bool)

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[autoscalingtypes.AutoScalingGroup, orphanAutoScalingGroup]{
		Headers:       []string{"AutoScalingGroup Name", "Min", "Desired", "Max", "Instances", "LBs", "TargetGroups"},
		ResourceLabel: "Auto Scaling Groups",
		List: func(ctx context.Context, emit func(autoscalingtypes.AutoScalingGroup) error) error {
			p := autoscaling.NewDescribeAutoScalingGroupsPaginator(a.AWSClient.ASG, &autoscaling.DescribeAutoScalingGroupsInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, asg := range page.AutoScalingGroups {
					if err := emit(asg); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, asg autoscalingtypes.AutoScalingGroup) (*orphanAutoScalingGroup, error) {
			name := aws.ToString(asg.AutoScalingGroupName)
			if !matchesFilterValue(name, filterByName) {
				return nil, nil
			}
			instanceCount := len(asg.Instances)
			lbCount := len(asg.LoadBalancerNames)
			tgCount := len(asg.TargetGroupARNs)
			desired := aws.ToInt32(asg.DesiredCapacity)
			min := aws.ToInt32(asg.MinSize)
			max := aws.ToInt32(asg.MaxSize)
			if instanceCount != 0 || desired != 0 || min != 0 || lbCount != 0 || tgCount != 0 {
				return nil, nil
			}
			return &orphanAutoScalingGroup{
				name:         name,
				min:          min,
				desired:      desired,
				max:          max,
				instances:    instanceCount,
				lbs:          lbCount,
				targetGroups: tgCount,
			}, nil
		},
		ToRow: func(r orphanAutoScalingGroup) []any {
			return []any{r.name, r.min, r.desired, r.max, r.instances, r.lbs, r.targetGroups}
		},
		Delete: func(ctx context.Context, r orphanAutoScalingGroup) error {
			a.Logger.LogInfo("Deleting AutoScalingGroup", map[string]any{"AutoScalingGroupName": r.name, "ForceDelete": force})
			_, err := a.AWSClient.ASG.DeleteAutoScalingGroup(ctx, &autoscaling.DeleteAutoScalingGroupInput{
				AutoScalingGroupName: aws.String(r.name),
				ForceDelete:          aws.Bool(force),
			})
			return err
		},
	})
}
