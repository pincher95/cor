/*
Copyright 2024 Elastic Scaler Contributors.

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
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// targetgroupsCmd represents the targetgroups command
var targetgroupsCmd = &cobra.Command{
	Use:   "targetgroups",
	Short: "Return orphaned ELBv2 target groups (not attached to any load balancer)",
	Long:  `Find and optionally delete ELBv2 target groups whose LoadBalancerArns list is empty.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout
		ctx := cmd.Context()

		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		additionalFlags := []flags.Flag{
			{Name: "filter-by-name", Type: "string"},
			{Name: "include-attached", Type: "bool"},
		}
		flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
		if err != nil {
			return err
		}

		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
			Profile:    aws.String((*flagValues)["profile"].(string)),
			Region:     aws.String((*flagValues)["region"].(string)),
		}
		cfg, err := handlers.NewConfig(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			return err
		}

		elbClient := elasticloadbalancingv2.NewFromConfig(*cfg)
		ec2Client := ec2.NewFromConfig(*cfg) // only used for account context consistency

		awsClient := &handlers.AWSClientImpl{
			ELB: elbClient,
			EC2: ec2Client,
		}

		return runTargetGroupsCmd(ctx, prompterClient, output, awsClient, flagValues)
	},
}

func init() {
	targetgroupsCmd.Flags().String("filter-by-name", "", "Filter by target group name (substring match).")
	targetgroupsCmd.Flags().Bool("include-attached", false, "Include target groups attached to load balancers (default: show only orphans).")
}

func runTargetGroupsCmd(ctx context.Context, prompter prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  prompter,
		Output:    output,
	}
	return command.executeTargetGroups(ctx, flagValues)
}

func (t *AWSCommand) executeTargetGroups(ctx context.Context, flagValues *map[string]any) error {
	// If deleting, confirm up-front so we can stream without buffering IDs.
	doDelete := false
	if (*flagValues)["delete"].(bool) {
		confirm, err := t.Prompter.Confirm("Are you sure you want to proceed? (yes/no): ")
		if err != nil {
			t.Logger.LogError("Error during user prompt", err, nil, false)
			return err
		}
		if confirm == nil || !*confirm {
			t.Logger.LogInfo("Aborted.", nil)
			return nil
		}
		doDelete = true
	}

	tgChan := make(chan elbtypes.TargetGroup, 50)
	resultsChan := make(chan table.Row, 50)

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		p := elasticloadbalancingv2.NewDescribeTargetGroupsPaginator(t.AWSClient.ELB, &elasticloadbalancingv2.DescribeTargetGroupsInput{})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
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

	filterByName := (*flagValues)["filter-by-name"].(string)
	includeAttached := (*flagValues)["include-attached"].(bool)

	for range NumGoroutines {
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
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
	printDone := make(chan error, 1)
	go func() {
		stream := printer.NewStreamTable(t.Output, true, []string{"TargetGroup Name", "TargetGroup ARN", "TargetType", "Protocol", "Port", "VPC ID", "Attached LBs"})
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
		finish := func(err error) {
			stream.Close()
			printDone <- err
		}

		for row := range resultsChan {
			stream.WriteRow(row...)

			if doDelete {
				// Name, Arn, Type, Protocol, Port, VpcId, AttachedCount
				if len(row) < 7 {
					continue
				}
				arn, _ := row[1].(string)
				attachedCount, _ := row[6].(int)
				if attachedCount > 0 {
					t.Logger.LogInfo("Skipping attached target group", map[string]any{"TargetGroupArn": arn, "AttachedLoadBalancers": attachedCount})
					continue
				}
				if arn == "" || arn == "-" {
					continue
				}
				t.Logger.LogInfo("Deleting target group", map[string]any{"TargetGroupArn": arn})
				if _, err := t.AWSClient.ELB.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{
					TargetGroupArn: aws.String(arn),
				}); err != nil {
					finish(err)
					return
				}
			}
		}
		finish(nil)
	}()

	if err := g.Wait(); err != nil {
		t.Logger.LogError("Error during target group processing", err, nil, false)
		return err
	}
	close(resultsChan)
	if err := <-printDone; err != nil {
		t.Logger.LogError("Error streaming/deleting target groups", err, nil, false)
		return err
	}

	return nil
}

// Legacy pretty-table printer removed in favor of streaming output for low memory usage.
