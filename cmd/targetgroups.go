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
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

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
	// Preserve the original context for delete operations (avoid errgroup ctx cancellation).
	rootCtx := ctx

	collectDeletes := (*flagValues)["delete"].(bool)

	tgChan := make(chan elbtypes.TargetGroup, 50)
	resultsChan := make(chan table.Row, 50)

	g, egCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		p := elasticloadbalancingv2.NewDescribeTargetGroupsPaginator(t.AWSClient.ELB, &elasticloadbalancingv2.DescribeTargetGroupsInput{})
		for p.HasMorePages() {
			page, err := p.NextPage(egCtx)
			if err != nil {
				close(tgChan)
				return err
			}
			for _, tg := range page.TargetGroups {
				tgChan <- tg
			}
		}
		close(tgChan)
		return nil
	})

	filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))
	includeAttached := (*flagValues)["include-attached"].(bool)

	for range NumGoroutines {
		g.Go(func() error {
			for {
				select {
				case <-egCtx.Done():
					return egCtx.Err()
				case tg, ok := <-tgChan:
					if !ok {
						return nil
					}

					name := aws.ToString(tg.TargetGroupName)
					if filterByName != "" && !strings.Contains(name, filterByName) {
						continue
					}

					attachedCount := 0
					if tg.LoadBalancerArns != nil {
						attachedCount = len(tg.LoadBalancerArns)
					}
					if !includeAttached && attachedCount > 0 {
						continue
					}

					arn := aws.ToString(tg.TargetGroupArn)
					if arn == "" {
						continue
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

					resultsChan <- table.Row{name, arn, tgType, proto, port, vpcID, attachedCount}
				}
			}
		})
	}

	// Stream output + optional delete
	printDone := make(chan []string, 1)
	go func() {
		stream := printer.NewStreamTable(t.Output, true, []string{"TargetGroup Name", "TargetGroup ARN", "TargetType", "Protocol", "Port", "VPC ID", "Attached LBs"})
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
		deleteArns := make([]string, 0)
		finish := func() {
			stream.Close()
			printDone <- deleteArns
		}

		for row := range resultsChan {
			stream.WriteRow(row...)

			if collectDeletes {
				// Name, Arn, Type, Protocol, Port, VpcId, AttachedCount
				if len(row) < 7 {
					continue
				}
				arn, _ := row[1].(string)
				attachedCount, _ := row[6].(int)
				if attachedCount > 0 {
					continue
				}
				if arn == "" || arn == "-" {
					continue
				}
				deleteArns = append(deleteArns, arn)
			}
		}
		finish()
	}()

	if err := g.Wait(); err != nil {
		t.Logger.LogError("Error during target group processing", err, nil, false)
		return err
	}
	close(resultsChan)
	deleteArns := <-printDone
	if collectDeletes {
		if len(deleteArns) == 0 {
			return nil
		}
		confirm, err := confirmDelete(t.Prompter, t.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, arn := range deleteArns {
			t.Logger.LogInfo("Deleting target group", map[string]any{"TargetGroupArn": arn})
			if _, err := t.AWSClient.ELB.DeleteTargetGroup(rootCtx, &elasticloadbalancingv2.DeleteTargetGroupInput{
				TargetGroupArn: aws.String(arn),
			}); err != nil {
				t.Logger.LogError("Error deleting target group", err, map[string]any{"TargetGroupArn": arn}, false)
				return err
			}
		}
	}

	return nil
}

// Legacy pretty-table printer removed in favor of streaming output for low memory usage.
