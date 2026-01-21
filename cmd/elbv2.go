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
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// elbv2Cmd represents the elbv2 command
var elbv2Cmd = &cobra.Command{
	Use:   "elbv2",
	Short: "Return Elastic LoadBalancer of type Application/Network",
	Long:  ``,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Create prompter using the prompter package
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout

		// Create a context
		ctx := cmd.Context()

		// Get the flags from the command and also the additional flags specific to this command
		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		// Specify additional flags that are specific to this command
		additionalFlags := []flags.Flag{
			{
				Name: "filter-by-name",
				Type: "string",
			},
		}
		// Get the flags
		flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
		if err != nil {
			return err
		}

		// Create AWS client
		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
			Profile:    aws.String((*flagValues)["profile"].(string)),
			Region:     aws.String((*flagValues)["region"].(string)),
		}
		cfg, err := handlers.NewConfig(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			return err
		}

		// Create an instance of ELBV2 client
		elbClient := elasticloadbalancingv2.NewFromConfig(*cfg)
		ec2Client := ec2.NewFromConfig(*cfg)

		awsClient := &handlers.AWSClientImpl{
			ELB: elbClient,
			EC2: ec2Client,
		}

		return runElbv2Cmd(ctx, prompterClient, output, awsClient, flagValues)
	},
}

func init() {
	elbv2Cmd.Flags().String("filter-by-name", "", "Filter load balancers by name (substring match).")
}

func runElbv2Cmd(ctx context.Context, prompter prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	// Create an instance of elbv2Command
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  prompter,
		Output:    output,
	}

	return command.executeElbv2(ctx, flagValues)
}

func (e *AWSCommand) executeElbv2(ctx context.Context, flagValues *map[string]any) error {
	// Preserve the original context for delete operations (avoid errgroup ctx cancellation).
	rootCtx := ctx

	collectDeletes := (*flagValues)["delete"].(bool)
	type deleteCandidate struct {
		lbName  string
		lbArn   string
		tgNames []string
	}

	// Create channels to send load balancers
	loadBalancerChan := make(chan types.LoadBalancer, 50)
	resultsChan := make(chan table.Row, 50)

	// Create an errgroup with context
	g, egCtx := errgroup.WithContext(ctx)
	filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))

	// Goroutine to describe load balancers
	g.Go(func() error {
		if err := e.describeLoadBalancersV2(egCtx, loadBalancerChan); err != nil {
			return err
		}
		return nil
	})

	for range NumGoroutines {
		g.Go(func() error {
			for {
				select {
				case <-egCtx.Done():
					return egCtx.Err()
				case lb, ok := <-loadBalancerChan:
					if !ok {
						return nil
					}
					tableRow, err := e.handleLoadBalancerV2(egCtx, lb, filterByName)
					if err != nil {
						return err
					}
					if tableRow != nil {
						resultsChan <- *tableRow
					}
				}
			}
		})
	}

	// Stream output + optional delete
	type printResult struct {
		err     error
		deletes []deleteCandidate
	}
	printDone := make(chan printResult, 1)
	go func() {
		stream := printer.NewStreamTable(e.Output, true, []string{"LoadBalancer Name", "LoadBalancer ARN", "targetGroups without targets"})
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
		deleteCandidates := make([]deleteCandidate, 0)
		finish := func(err error) {
			stream.Close()
			printDone <- printResult{err: err, deletes: deleteCandidates}
		}

		for row := range resultsChan {
			stream.WriteRow(row...)

			if collectDeletes {
				// row: Name, Arn, targetGroupNames (newline separated)
				if len(row) < 3 {
					continue
				}
				lbName, _ := row[0].(string)
				lbArn, _ := row[1].(string)
				tgNames, _ := row[2].(string)
				if lbArn == "" {
					continue
				}
				deleteCandidates = append(deleteCandidates, deleteCandidate{
					lbName:  lbName,
					lbArn:   lbArn,
					tgNames: strings.Split(tgNames, "\n"),
				})
			}
		}
		finish(nil)
	}()

	// Wait for the describer and workers to finish.
	err := g.Wait()
	// Close results channel in all cases so the printer goroutine can exit.
	close(resultsChan)
	printRes := <-printDone
	if printRes.err != nil {
		e.Logger.LogError("Error streaming elbv2", printRes.err, nil, false)
		if err == nil {
			err = printRes.err
		}
	}
	if err != nil {
		e.Logger.LogError("Error during elbv2 processing", err, nil, false)
		return err
	}

	if collectDeletes {
		if len(printRes.deletes) == 0 {
			return nil
		}
		confirm, err := confirmDelete(e.Prompter, e.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, candidate := range printRes.deletes {
			e.Logger.LogInfo("Deleting LoadBalancer", map[string]any{"LoadBalancerName": candidate.lbName})
			if err := e.deleteListeners(rootCtx, aws.String(candidate.lbArn)); err != nil {
				return err
			}
			if err := e.deleteTargetGroups(rootCtx, candidate.tgNames); err != nil {
				return err
			}
			if _, err := e.AWSClient.DeleteLoadBalancer(rootCtx, &elasticloadbalancingv2.DeleteLoadBalancerInput{
				LoadBalancerArn: aws.String(candidate.lbArn),
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

func (e *AWSCommand) describeLoadBalancersV2(ctx context.Context, loadBalancerChan chan<- types.LoadBalancer) error {
	// Create a paginator to describe load balancers and send them to the channel
	paginator := elasticloadbalancingv2.NewDescribeLoadBalancersPaginator(e.AWSClient.ELB, &elasticloadbalancingv2.DescribeLoadBalancersInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, lb := range page.LoadBalancers {
			loadBalancerChan <- lb
		}
	}
	close(loadBalancerChan)
	return nil
}

func (e *AWSCommand) handleLoadBalancerV2(ctx context.Context, elb types.LoadBalancer, filterByName string) (*table.Row, error) {
	// Early exit if the load balancer name doesn't match the filter
	if filterByName != "" && !strings.Contains(*elb.LoadBalancerName, filterByName) {
		return nil, nil
	}

	// Get target groups for the load balancer
	targetGroups, err := e.getTargetGroups(ctx, elb.LoadBalancerArn)
	if err != nil {
		return nil, err
	}

	// Process the load balancer and its target groups
	row, err := e.processLoadBalancer(ctx, elb, targetGroups)
	if err != nil {
		return nil, err
	}

	return row, nil
}

func (e *AWSCommand) processLoadBalancer(ctx context.Context, elb types.LoadBalancer, targetGroups []types.TargetGroup) (*table.Row, error) {
	// Define a structure for target group results
	type tgResult struct {
		tgName         string
		hasValidTarget bool // true if at least one target in the group is healthy, from delegation, or attached to a valid EC2 instance
	}

	// Create a buffered channel sized to the number of target groups so sends do not block.
	resChan := make(chan tgResult, len(targetGroups))

	// Process each target group concurrently using an errgroup.
	g, ctx := errgroup.WithContext(ctx)
	for _, tg := range targetGroups {
		tg := tg // capture loop variable
		g.Go(func() error {
			resp, err := e.AWSClient.DescribeTargetHealth(ctx, &elasticloadbalancingv2.DescribeTargetHealthInput{
				TargetGroupArn: tg.TargetGroupArn,
			})
			if err != nil {
				return err
			}

			validFound := false
			// For each target health description, check if it's healthy.
			// If unhealthy, first check if the target IP is within the delegated range,
			// and if not, then verify via EC2 if the target exists.
			for _, desc := range resp.TargetHealthDescriptions {
				if desc.TargetHealth.State == types.TargetHealthStateEnumHealthy {
					validFound = true
					break
				}
				exists, err := e.checkInstanceExists(ctx, *desc.Target.Id)
				if err != nil {
					continue
				}
				if exists {
					validFound = true
					break
				}
			}

			resChan <- tgResult{tgName: *tg.TargetGroupName, hasValidTarget: validFound}
			return nil
		})
	}

	// Wait for all goroutines to finish
	if err := g.Wait(); err != nil {
		return nil, err
	}
	close(resChan)

	// Gather results from the channel.
	var results []tgResult
	for res := range resChan {
		results = append(results, res)
	}

	// If any target group has a valid target, skip this load balancer.
	for _, r := range results {
		if r.hasValidTarget {
			return nil, nil
		}
	}

	// Otherwise, join the names of target groups that do not have valid targets.
	var tgNames []string
	for _, r := range results {
		tgNames = append(tgNames, r.tgName)
	}

	if len(tgNames) > 0 {
		return &table.Row{
			*elb.LoadBalancerName,
			*elb.LoadBalancerArn,
			strings.Join(tgNames, "\n"),
		}, nil
	}
	return nil, nil
}

func (e *AWSCommand) getTargetGroups(ctx context.Context, loadBalancerArn *string) ([]types.TargetGroup, error) {
	var targetGroups []types.TargetGroup
	paginator := elasticloadbalancingv2.NewDescribeTargetGroupsPaginator(e.AWSClient.ELB, &elasticloadbalancingv2.DescribeTargetGroupsInput{
		LoadBalancerArn: loadBalancerArn,
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		targetGroups = append(targetGroups, page.TargetGroups...)
	}

	return targetGroups, nil
}

func (e *AWSCommand) deleteListeners(ctx context.Context, loadBalancerArn *string) error {
	listenerPaginator := elasticloadbalancingv2.NewDescribeListenersPaginator(e.AWSClient.ELB, &elasticloadbalancingv2.DescribeListenersInput{
		LoadBalancerArn: loadBalancerArn,
	})

	for listenerPaginator.HasMorePages() {
		listenerPage, err := listenerPaginator.NextPage(ctx)
		if err != nil {
			return err
		}

		for _, listener := range listenerPage.Listeners {
			e.Logger.LogInfo("Deleting", map[string]any{"ListenerArn": *listener.ListenerArn})
			_, err := e.AWSClient.DeleteListener(ctx, &elasticloadbalancingv2.DeleteListenerInput{
				ListenerArn: listener.ListenerArn,
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *AWSCommand) deleteTargetGroups(ctx context.Context, targetGroupNames []string) error {
	targerGroups, err := e.AWSClient.DescribeTargetGroups(ctx, &elasticloadbalancingv2.DescribeTargetGroupsInput{
		Names: targetGroupNames,
	})
	if err != nil {
		return err
	}

	for _, target := range targerGroups.TargetGroups {
		e.Logger.LogInfo("Deleting", map[string]any{"TargetGroupName": *target.TargetGroupName})
		_, err = e.AWSClient.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{
			TargetGroupArn: target.TargetGroupArn,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Legacy pretty-table printer removed in favor of streaming output for low memory usage.

// checkInstanceExists verifies via the EC2 API whether an instance exists with the provided IP address.
// If no instance is found, the target is considered invalid.
func (e *AWSCommand) checkInstanceExists(ctx context.Context, targetIP string) (bool, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("private-ip-address"),
				Values: []string{targetIP},
			},
		},
	}
	result, err := e.AWSClient.EC2.DescribeInstances(ctx, input)
	if err != nil {
		return false, err
	}

	for _, reservation := range result.Reservations {
		if len(reservation.Instances) > 0 {
			return true, nil
		}
	}
	return false, nil
}
