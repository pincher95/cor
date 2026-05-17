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
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanLBv1 struct {
	name              string
	listenerCount     int
	missingTargets    string
	unhealthyTargets  string
	vpcID             string
	tags              string
	isOrphan          bool
	deleteCandidateID string
}

type elbv1Evaluation struct {
	noTargets           bool
	orphanTargets       []string
	unhealthyTargets    []string
	hasExistingTargets  bool
	hasUnhealthyTargets bool
}

var elbv1Cmd = &cobra.Command{
	Use:   "elbv1",
	Short: "Return ELB of type Classic",
	Long:  `Return Classic ELB with instance state unhealthy.`,
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
					ELBv1: elasticloadbalancing.NewFromConfig(*cfg),
					EC2:   ec2.NewFromConfig(*cfg),
				}
			},
		}, (*AWSCommand).executeElbv1)
	},
}

func init() {
	elbv1Cmd.Flags().String("filter-by-name", "", "Filter load balancers by name (supports * and ?).")
	elbv1Cmd.Flags().String("filter-by-tags", "", "Filter by tags (key=value or key; comma-separated).")
	elbv1Cmd.Flags().Bool("show-unhealthy", false, "Include load balancers with unhealthy targets.")
	elbv1Cmd.Flags().Bool("show-tags", false, "Include tags column in output.")
}

func (a *AWSCommand) executeElbv1(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	showUnhealthy := (*extras)["show-unhealthy"].(bool)
	showTags := (*extras)["show-tags"].(bool)
	filterByName := normalizeFilterValue((*extras)["filter-by-name"].(string))
	tagFilters := parseTagFilters((*extras)["filter-by-tags"].(string))

	headers := []string{"LoadBalancer Name", "number of listeners", "targets without instances"}
	if showUnhealthy {
		headers = append(headers, "targets unhealthy")
	}
	headers = append(headers, "VPC ID")
	if showTags {
		headers = append(headers, "Tags")
	}

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[types.LoadBalancerDescription, orphanLBv1]{
		Headers:       headers,
		ResourceLabel: "Classic ELBs",
		List: func(ctx context.Context, emit func(types.LoadBalancerDescription) error) error {
			p := elasticloadbalancing.NewDescribeLoadBalancersPaginator(a.AWSClient.ELBv1, &elasticloadbalancing.DescribeLoadBalancersInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, lb := range page.LoadBalancerDescriptions {
					if err := emit(lb); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, lb types.LoadBalancerDescription) (*orphanLBv1, error) {
			lbName := aws.ToString(lb.LoadBalancerName)
			if !matchesFilterValue(lbName, filterByName) {
				return nil, nil
			}

			needTags := showTags || len(tagFilters) > 0
			tagsValue := "-"
			if needTags {
				tagsMap, formatted, err := a.describeClassicElbTags(ctx, lbName)
				if err != nil {
					return nil, err
				}
				if len(tagFilters) > 0 && !tagsMatchFilters(tagsMap, tagFilters) {
					return nil, nil
				}
				tagsValue = formatted
			}

			eval, err := a.evaluateElbv1(ctx, &lb)
			if err != nil {
				return nil, err
			}
			if eval == nil {
				return nil, nil
			}
			isOrphan := eval.noTargets || (!eval.hasExistingTargets && len(eval.orphanTargets) > 0)
			shouldOutput := isOrphan || (showUnhealthy && eval.hasUnhealthyTargets)
			if !shouldOutput {
				return nil, nil
			}

			missingTargets := "-"
			if len(eval.orphanTargets) > 0 {
				missingTargets = strings.Join(eval.orphanTargets, "\n")
			}
			unhealthyTargets := "-"
			if len(eval.unhealthyTargets) > 0 {
				unhealthyTargets = strings.Join(eval.unhealthyTargets, "\n")
			}
			vpcID := aws.ToString(lb.VPCId)
			if vpcID == "" {
				vpcID = "-"
			}
			deleteID := ""
			if isOrphan {
				deleteID = lbName
			}
			return &orphanLBv1{
				name:              lbName,
				listenerCount:     len(lb.ListenerDescriptions),
				missingTargets:    missingTargets,
				unhealthyTargets:  unhealthyTargets,
				vpcID:             vpcID,
				tags:              tagsValue,
				isOrphan:          isOrphan,
				deleteCandidateID: deleteID,
			}, nil
		},
		ToRow: func(r orphanLBv1) []any {
			row := []any{r.name, r.listenerCount, r.missingTargets}
			if showUnhealthy {
				row = append(row, r.unhealthyTargets)
			}
			row = append(row, r.vpcID)
			if showTags {
				row = append(row, r.tags)
			}
			return row
		},
		Delete: func(ctx context.Context, r orphanLBv1) error {
			if r.deleteCandidateID == "" {
				return nil
			}
			a.Logger.LogInfo("Deleting LoadBalancer", map[string]any{"LoadBalancerName": r.deleteCandidateID})
			_, err := a.AWSClient.ELBv1.DeleteLoadBalancer(ctx, &elasticloadbalancing.DeleteLoadBalancerInput{
				LoadBalancerName: aws.String(r.deleteCandidateID),
			})
			return err
		},
	})
}

func (a *AWSCommand) evaluateElbv1(ctx context.Context, lb *types.LoadBalancerDescription) (*elbv1Evaluation, error) {
	eval := &elbv1Evaluation{}
	if len(lb.Instances) == 0 {
		eval.noTargets = true
		return eval, nil
	}

	instanceHealth, err := a.AWSClient.ELBv1.DescribeInstanceHealth(ctx, &elasticloadbalancing.DescribeInstanceHealthInput{
		LoadBalancerName: lb.LoadBalancerName,
	})
	if err != nil {
		return nil, err
	}
	if len(instanceHealth.InstanceStates) == 0 {
		eval.noTargets = true
		return eval, nil
	}

	for _, instanceState := range instanceHealth.InstanceStates {
		instanceID := aws.ToString(instanceState.InstanceId)
		state := aws.ToString(instanceState.State)
		if state == "InService" {
			eval.hasExistingTargets = true
			continue
		}
		if instanceID != "" {
			eval.hasUnhealthyTargets = true
			eval.unhealthyTargets = append(eval.unhealthyTargets, instanceID)
		}
		exists, err := a.checkClassicInstanceExists(ctx, instanceID)
		if err != nil {
			continue
		}
		if exists {
			eval.hasExistingTargets = true
			continue
		}
		if instanceID != "" {
			eval.orphanTargets = append(eval.orphanTargets, instanceID)
		}
	}
	return eval, nil
}

func (a *AWSCommand) checkClassicInstanceExists(ctx context.Context, instanceID string) (bool, error) {
	if instanceID == "" {
		return false, nil
	}
	result, err := a.AWSClient.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("instance-id"),
				Values: []string{instanceID},
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

func (a *AWSCommand) describeClassicElbTags(ctx context.Context, loadBalancerName string) (map[string]string, string, error) {
	if loadBalancerName == "" {
		return map[string]string{}, "-", nil
	}
	resp, err := a.AWSClient.ELBv1.DescribeTags(ctx, &elasticloadbalancing.DescribeTagsInput{
		LoadBalancerNames: []string{loadBalancerName},
	})
	if err != nil {
		return nil, "", err
	}
	for _, desc := range resp.TagDescriptions {
		if aws.ToString(desc.LoadBalancerName) == loadBalancerName {
			tagMap := elbTagsToMap(desc.Tags, func(t types.Tag) *string { return t.Key }, func(t types.Tag) *string { return t.Value })
			return tagMap, formatElbTags(tagMap), nil
		}
	}
	return map[string]string{}, "-", nil
}
