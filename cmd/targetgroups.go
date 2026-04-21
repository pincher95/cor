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
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanTargetGroup struct {
	name, arn, targetType, protocol, vpcID string
	port                                   int32
	attached                               int
}

// targetgroupsCmd represents the targetgroups command
var targetgroupsCmd = &cobra.Command{
	Use:   "targetgroups",
	Short: "Return orphaned ELBv2 target groups (not attached to any load balancer)",
	Long:  `Find and optionally delete ELBv2 target groups whose LoadBalancerArns list is empty.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "include-attached", Type: "bool"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{
					ELB: elasticloadbalancingv2.NewFromConfig(*cfg),
					EC2: ec2.NewFromConfig(*cfg),
				}
			},
		}, (*AWSCommand).executeTargetGroups)
	},
}

func init() {
	targetgroupsCmd.Flags().String("filter-by-name", "", "Filter by target group name (substring match).")
	targetgroupsCmd.Flags().Bool("include-attached", false, "Include target groups attached to load balancers (default: show only orphans).")
}

func (t *AWSCommand) executeTargetGroups(ctx context.Context, flagValues *map[string]any) error {
	filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))
	includeAttached := (*flagValues)["include-attached"].(bool)

	return runOrphanPipeline(t, ctx, flagValues, OrphanPipeline[elbtypes.TargetGroup, orphanTargetGroup]{
		Headers: []string{"TargetGroup Name", "TargetGroup ARN", "TargetType", "Protocol", "Port", "VPC ID", "Attached LBs"},
		List: func(ctx context.Context, emit func(elbtypes.TargetGroup) error) error {
			p := elasticloadbalancingv2.NewDescribeTargetGroupsPaginator(t.AWSClient.ELB, &elasticloadbalancingv2.DescribeTargetGroupsInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, tg := range page.TargetGroups {
					if err := emit(tg); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, tg elbtypes.TargetGroup) (*orphanTargetGroup, error) {
			name := aws.ToString(tg.TargetGroupName)
			if filterByName != "" && !strings.Contains(name, filterByName) {
				return nil, nil
			}
			attachedCount := 0
			if tg.LoadBalancerArns != nil {
				attachedCount = len(tg.LoadBalancerArns)
			}
			if !includeAttached && attachedCount > 0 {
				return nil, nil
			}
			arn := aws.ToString(tg.TargetGroupArn)
			if arn == "" {
				return nil, nil
			}
			vpcID := aws.ToString(tg.VpcId)
			if vpcID == "" {
				vpcID = "-"
			}
			proto := string(tg.Protocol)
			if proto == "" {
				proto = "-"
			}
			port := int32(0)
			if tg.Port != nil {
				port = *tg.Port
			}
			tgType := string(tg.TargetType)
			if tgType == "" {
				tgType = "-"
			}
			return &orphanTargetGroup{
				name:       name,
				arn:        arn,
				targetType: tgType,
				protocol:   proto,
				vpcID:      vpcID,
				port:       port,
				attached:   attachedCount,
			}, nil
		},
		ToRow: func(r orphanTargetGroup) []any {
			return []any{r.name, r.arn, r.targetType, r.protocol, r.port, r.vpcID, r.attached}
		},
		Delete: func(ctx context.Context, r orphanTargetGroup) error {
			t.Logger.LogInfo("Deleting target group", map[string]any{"TargetGroupArn": r.arn})
			_, err := t.AWSClient.ELB.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(r.arn)})
			return err
		},
	})
}

// Legacy pretty-table printer removed in favor of streaming output for low memory usage.
