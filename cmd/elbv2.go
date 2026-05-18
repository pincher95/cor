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
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

type orphanLBv2 struct {
	lbName        string
	lbArn         string
	lbType        types.LoadBalancerTypeEnum
	orphanTGs     string
	unhealthyTGs  string
	tags          string
	isOrphan      bool
	orphanTGNames []string
}

type lbEvaluation struct {
	orphanTargetGroups    []string
	unhealthyTargetGroups []string
	hasExistingTargets    bool
	hasUnhealthyTargets   bool
}

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

func (a *AWSCommand) executeElbv2(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	showUnhealthy := (*extras)["show-unhealthy"].(bool)
	showTags := (*extras)["show-tags"].(bool)
	filterByName := normalizeFilterValue((*extras)["filter-by-name"].(string))
	tagFilters := parseTagFilters((*extras)["filter-by-tags"].(string))

	headers := []string{"LoadBalancer Name", "LoadBalancer ARN", "targetGroups without targets"}
	if showUnhealthy {
		headers = append(headers, "targetGroups with unhealthy targets")
	}
	if showTags {
		headers = append(headers, "Tags")
	}

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[types.LoadBalancer, orphanLBv2]{
		Headers:       headers,
		ResourceLabel: "ELBv2 load balancers",
		List: func(ctx context.Context, emit func(types.LoadBalancer) error) error {
			p := elasticloadbalancingv2.NewDescribeLoadBalancersPaginator(a.AWSClient.ELB, &elasticloadbalancingv2.DescribeLoadBalancersInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, lb := range page.LoadBalancers {
					if err := emit(lb); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, lb types.LoadBalancer) (*orphanLBv2, error) {
			if !matchesFilterValue(aws.ToString(lb.LoadBalancerName), filterByName) {
				return nil, nil
			}
			needTags := showTags || len(tagFilters) > 0
			tagsValue := "-"
			if needTags {
				tagsMap, formatted, err := a.describeElbv2Tags(ctx, aws.ToString(lb.LoadBalancerArn))
				if err != nil {
					return nil, err
				}
				if len(tagFilters) > 0 && !tagsMatchFilters(tagsMap, tagFilters) {
					return nil, nil
				}
				tagsValue = formatted
			}

			targetGroups, err := a.getTargetGroups(ctx, lb.LoadBalancerArn)
			if err != nil {
				return nil, err
			}
			eval, err := a.processLoadBalancer(ctx, targetGroups)
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
			return &orphanLBv2{
				lbName:        aws.ToString(lb.LoadBalancerName),
				lbArn:         aws.ToString(lb.LoadBalancerArn),
				lbType:        lb.Type,
				orphanTGs:     strings.Join(eval.orphanTargetGroups, "\n"),
				unhealthyTGs:  strings.Join(eval.unhealthyTargetGroups, "\n"),
				tags:          tagsValue,
				isOrphan:      isOrphan,
				orphanTGNames: eval.orphanTargetGroups,
			}, nil
		},
		ToRow: func(r orphanLBv2) []any {
			row := []any{r.lbName, r.lbArn, r.orphanTGs}
			if showUnhealthy {
				row = append(row, r.unhealthyTGs)
			}
			if showTags {
				row = append(row, r.tags)
			}
			return row
		},
		Delete: func(ctx context.Context, r orphanLBv2) error {
			if !r.isOrphan {
				return nil
			}
			a.Logger.LogInfo("Deleting LoadBalancer", map[string]any{"LoadBalancerName": r.lbName})
			if err := a.deleteListeners(ctx, aws.String(r.lbArn)); err != nil {
				return err
			}
			if err := a.deleteTargetGroups(ctx, r.orphanTGNames); err != nil {
				return err
			}
			_, err := a.AWSClient.ELB.DeleteLoadBalancer(ctx, &elasticloadbalancingv2.DeleteLoadBalancerInput{
				LoadBalancerArn: aws.String(r.lbArn),
			})
			return err
		},
		MonthlyCost: func(r orphanLBv2) cost.USD {
			switch r.lbType {
			case types.LoadBalancerTypeEnumApplication, types.LoadBalancerTypeEnumNetwork:
				return cost.USD(cost.HoursPerMonth) * a.Pricing.ALBHour()
			case types.LoadBalancerTypeEnumGateway:
				return cost.USD(cost.HoursPerMonth) * a.Pricing.GatewayLBHour()
			}
			return 0
		},
	})
}

func (a *AWSCommand) processLoadBalancer(ctx context.Context, targetGroups []types.TargetGroup) (*lbEvaluation, error) {
	if len(targetGroups) == 0 {
		return nil, nil
	}

	type tgResult struct {
		tgName             string
		hasExistingTarget  bool
		hasUnhealthyTarget bool
		isOrphan           bool
	}
	resChan := make(chan tgResult, len(targetGroups))

	g, gctx := errgroup.WithContext(ctx)
	for _, tg := range targetGroups {
		g.Go(func() error {
			resp, err := a.AWSClient.ELB.DescribeTargetHealth(gctx, &elasticloadbalancingv2.DescribeTargetHealthInput{
				TargetGroupArn: tg.TargetGroupArn,
			})
			if err != nil {
				return err
			}
			hasExistingTarget := false
			hasUnhealthyTarget := false
			if len(resp.TargetHealthDescriptions) == 0 {
				resChan <- tgResult{tgName: aws.ToString(tg.TargetGroupName), isOrphan: true}
				return nil
			}
			for _, desc := range resp.TargetHealthDescriptions {
				if desc.TargetHealth.State == types.TargetHealthStateEnumHealthy {
					hasExistingTarget = true
					continue
				}
				hasUnhealthyTarget = true
				exists, err := a.checkInstanceExistsByTarget(gctx, aws.ToString(desc.Target.Id))
				if err != nil {
					continue
				}
				if exists {
					hasExistingTarget = true
				}
			}
			resChan <- tgResult{
				tgName:             aws.ToString(tg.TargetGroupName),
				hasExistingTarget:  hasExistingTarget,
				hasUnhealthyTarget: hasUnhealthyTarget,
				isOrphan:           !hasExistingTarget,
			}
			return nil
		})
	}
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

func (a *AWSCommand) getTargetGroups(ctx context.Context, loadBalancerArn *string) ([]types.TargetGroup, error) {
	var targetGroups []types.TargetGroup
	p := elasticloadbalancingv2.NewDescribeTargetGroupsPaginator(a.AWSClient.ELB, &elasticloadbalancingv2.DescribeTargetGroupsInput{
		LoadBalancerArn: loadBalancerArn,
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		targetGroups = append(targetGroups, page.TargetGroups...)
	}
	return targetGroups, nil
}

func (a *AWSCommand) deleteListeners(ctx context.Context, loadBalancerArn *string) error {
	p := elasticloadbalancingv2.NewDescribeListenersPaginator(a.AWSClient.ELB, &elasticloadbalancingv2.DescribeListenersInput{
		LoadBalancerArn: loadBalancerArn,
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, listener := range page.Listeners {
			a.Logger.LogInfo("Deleting", map[string]any{"ListenerArn": aws.ToString(listener.ListenerArn)})
			if _, err := a.AWSClient.ELB.DeleteListener(ctx, &elasticloadbalancingv2.DeleteListenerInput{
				ListenerArn: listener.ListenerArn,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *AWSCommand) deleteTargetGroups(ctx context.Context, targetGroupNames []string) error {
	if len(targetGroupNames) == 0 {
		return nil
	}
	out, err := a.AWSClient.ELB.DescribeTargetGroups(ctx, &elasticloadbalancingv2.DescribeTargetGroupsInput{
		Names: targetGroupNames,
	})
	if err != nil {
		return err
	}
	for _, tg := range out.TargetGroups {
		a.Logger.LogInfo("Deleting", map[string]any{"TargetGroupName": aws.ToString(tg.TargetGroupName)})
		if _, err := a.AWSClient.ELB.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{
			TargetGroupArn: tg.TargetGroupArn,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a *AWSCommand) describeElbv2Tags(ctx context.Context, loadBalancerArn string) (map[string]string, string, error) {
	if loadBalancerArn == "" {
		return map[string]string{}, "-", nil
	}
	resp, err := a.AWSClient.ELB.DescribeTags(ctx, &elasticloadbalancingv2.DescribeTagsInput{
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

func (a *AWSCommand) checkInstanceExistsByTarget(ctx context.Context, targetID string) (bool, error) {
	if targetID == "" {
		return false, nil
	}
	filterName := "private-ip-address"
	if strings.HasPrefix(targetID, "i-") {
		filterName = "instance-id"
	}
	result, err := a.AWSClient.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String(filterName),
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
