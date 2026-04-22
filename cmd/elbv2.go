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
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

type deleteCandidate struct {
	lbName  string
	lbArn   string
	tgNames []string
}

type lbResult struct {
	row             table.Row
	deleteCandidate *deleteCandidate
}

type lbEvaluation struct {
	orphanTargetGroups    []string
	unhealthyTargetGroups []string
	hasExistingTargets    bool
	hasUnhealthyTargets   bool
}

// elbv2Cmd represents the elbv2 command
var elbv2Cmd = &cobra.Command{
	Use:   "elbv2",
	Short: "Return Elastic LoadBalancer of type Application/Network",
	Long:  ``,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "filter-by-tags", Type: "string"},
				{Name: "show-unhealthy", Type: "bool"},
				{Name: "show-tags", Type: "bool"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{
					ELB: elasticloadbalancingv2.NewFromConfig(*cfg),
					EC2: ec2.NewFromConfig(*cfg),
				}
			},
		}, (*AWSCommand).executeElbv2)
	},
}

func init() {
	elbv2Cmd.Flags().String("filter-by-name", "", "Filter load balancers by name (substring match).")
	elbv2Cmd.Flags().String("filter-by-tags", "", "Filter by tags (key=value or key; comma-separated).")
	elbv2Cmd.Flags().Bool("show-unhealthy", false, "Include load balancers with unhealthy targets.")
	elbv2Cmd.Flags().Bool("show-tags", false, "Include tags column in output.")
}

func (e *AWSCommand) executeElbv2(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	// Preserve the original context for delete operations (avoid errgroup ctx cancellation).
	rootCtx := ctx

	collectDeletes := globals.Delete
	showUnhealthy := (*extras)["show-unhealthy"].(bool)
	showTags := (*extras)["show-tags"].(bool)
	tagFilters := parseTagFilters((*extras)["filter-by-tags"].(string))

	// Create channels to send load balancers
	loadBalancerChan := make(chan types.LoadBalancer, 50)
	resultsChan := make(chan lbResult, 50)

	// Create an errgroup with context
	g, egCtx := errgroup.WithContext(ctx)
	filterByName := normalizeFilterValue((*extras)["filter-by-name"].(string))

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
					tableRow, err := e.handleLoadBalancerV2(egCtx, lb, filterByName, showUnhealthy, showTags, tagFilters)
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
		headers := []string{"LoadBalancer Name", "LoadBalancer ARN", "targetGroups without targets"}
		if showUnhealthy {
			headers = append(headers, "targetGroups with unhealthy targets")
		}
		if showTags {
			headers = append(headers, "Tags")
		}
		stream := printer.NewStreamTable(e.Output, true, headers)
		stream.SetSort(globals.SortBy, globals.SortDesc)
		deleteCandidates := make([]deleteCandidate, 0)
		finish := func(err error) {
			stream.Close()
			printDone <- printResult{err: err, deletes: deleteCandidates}
		}

		for res := range resultsChan {
			stream.WriteRow(res.row...)

			if collectDeletes && res.deleteCandidate != nil {
				deleteCandidates = append(deleteCandidates, *res.deleteCandidate)
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

func (e *AWSCommand) handleLoadBalancerV2(ctx context.Context, elb types.LoadBalancer, filterByName string, showUnhealthy bool, showTags bool, tagFilters []tagFilter) (*lbResult, error) {
	// Early exit if the load balancer name doesn't match the filter
	if !matchesFilterValue(aws.ToString(elb.LoadBalancerName), filterByName) {
		return nil, nil
	}

	needTags := showTags || len(tagFilters) > 0
	tagsValue := "-"
	if needTags {
		tagsMap, formattedTags, err := e.describeElbv2Tags(ctx, aws.ToString(elb.LoadBalancerArn))
		if err != nil {
			return nil, err
		}
		if len(tagFilters) > 0 && !tagsMatchFilters(tagsMap, tagFilters) {
			return nil, nil
		}
		tagsValue = formattedTags
	}

	// Get target groups for the load balancer
	targetGroups, err := e.getTargetGroups(ctx, elb.LoadBalancerArn)
	if err != nil {
		return nil, err
	}

	// Process the load balancer and its target groups
	eval, err := e.processLoadBalancer(ctx, targetGroups)
	if err != nil {
		return nil, err
	}

	if eval == nil {
		return nil, nil
	}

	isOrphan := !eval.hasExistingTargets && len(eval.orphanTargetGroups) > 0
	shouldOutput := isOrphan || (showUnhealthy && eval.hasUnhealthyTargets)
	if !shouldOutput {
		return nil, nil
	}

	orphanTargets := strings.Join(eval.orphanTargetGroups, "\n")
	unhealthyTargets := strings.Join(eval.unhealthyTargetGroups, "\n")

	row := table.Row{
		*elb.LoadBalancerName,
		*elb.LoadBalancerArn,
		orphanTargets,
	}
	if showUnhealthy {
		row = append(row, unhealthyTargets)
	}
	if showTags {
		row = append(row, tagsValue)
	}

	var candidate *deleteCandidate
	if isOrphan {
		candidate = &deleteCandidate{
			lbName:  *elb.LoadBalancerName,
			lbArn:   *elb.LoadBalancerArn,
			tgNames: eval.orphanTargetGroups,
		}
	}

	return &lbResult{row: row, deleteCandidate: candidate}, nil
}

func (e *AWSCommand) processLoadBalancer(ctx context.Context, targetGroups []types.TargetGroup) (*lbEvaluation, error) {
	if len(targetGroups) == 0 {
		return nil, nil
	}

	// Define a structure for target group results
	type tgResult struct {
		tgName             string
		hasExistingTarget  bool
		hasUnhealthyTarget bool
		isOrphan           bool
	}

	// Create a buffered channel sized to the number of target groups so sends do not block.
	resChan := make(chan tgResult, len(targetGroups))

	// Process each target group concurrently using an errgroup.
	g, ctx := errgroup.WithContext(ctx)
	for _, tg := range targetGroups {
		g.Go(func() error {
			resp, err := e.AWSClient.DescribeTargetHealth(ctx, &elasticloadbalancingv2.DescribeTargetHealthInput{
				TargetGroupArn: tg.TargetGroupArn,
			})
			if err != nil {
				return err
			}

			hasExistingTarget := false
			hasUnhealthyTarget := false

			if len(resp.TargetHealthDescriptions) == 0 {
				resChan <- tgResult{tgName: *tg.TargetGroupName, isOrphan: true}
				return nil
			}

			// For each target health description, check if it's healthy.
			// If unhealthy, verify via EC2 if the target exists.
			for _, desc := range resp.TargetHealthDescriptions {
				if desc.TargetHealth.State == types.TargetHealthStateEnumHealthy {
					hasExistingTarget = true
					continue
				}
				hasUnhealthyTarget = true
				exists, err := e.checkInstanceExists(ctx, *desc.Target.Id)
				if err != nil {
					continue
				}
				if exists {
					hasExistingTarget = true
				}
			}

			resChan <- tgResult{
				tgName:             *tg.TargetGroupName,
				hasExistingTarget:  hasExistingTarget,
				hasUnhealthyTarget: hasUnhealthyTarget,
				isOrphan:           !hasExistingTarget,
			}
			return nil
		})
	}

	// Wait for all goroutines to finish
	if err := g.Wait(); err != nil {
		return nil, err
	}
	close(resChan)

	eval := &lbEvaluation{}
	for res := range resChan {
		if res.hasExistingTarget {
			eval.hasExistingTargets = true
		}
		if res.hasUnhealthyTarget {
			eval.hasUnhealthyTargets = true
			eval.unhealthyTargetGroups = append(eval.unhealthyTargetGroups, res.tgName)
		}
		if res.isOrphan {
			eval.orphanTargetGroups = append(eval.orphanTargetGroups, res.tgName)
		}
	}

	return eval, nil
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

func (e *AWSCommand) describeElbv2Tags(ctx context.Context, loadBalancerArn string) (map[string]string, string, error) {
	if loadBalancerArn == "" {
		return map[string]string{}, "-", nil
	}
	resp, err := e.AWSClient.ELB.DescribeTags(ctx, &elasticloadbalancingv2.DescribeTagsInput{
		ResourceArns: []string{loadBalancerArn},
	})
	if err != nil {
		return nil, "", err
	}
	for _, desc := range resp.TagDescriptions {
		if aws.ToString(desc.ResourceArn) == loadBalancerArn {
			tagMap := elbTagsToMap(desc.Tags, func(t types.Tag) *string { return t.Key }, func(t types.Tag) *string { return t.Value })
			return tagMap, formatElbTags(tagMap), nil
		}
	}
	return map[string]string{}, "-", nil
}

// checkInstanceExists verifies via the EC2 API whether an instance exists for a target ID or IP.
// If no instance is found, the target is considered invalid.
func (e *AWSCommand) checkInstanceExists(ctx context.Context, targetID string) (bool, error) {
	if strings.HasPrefix(targetID, "i-") {
		result, err := e.AWSClient.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{
				{
					Name:   aws.String("instance-id"),
					Values: []string{targetID},
				},
			},
		})
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

	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("private-ip-address"),
				Values: []string{targetID},
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
