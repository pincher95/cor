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
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing/types"
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
)

// elbv1Cmd represents the elbv1 command
var elbv1Cmd = &cobra.Command{
	Use:   "elbv1",
	Short: "Return ELB of type Classic",
	Long:  `Return Classic ELB with instance state unhealthy.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		// Create a new logger and error handler
		logger := logging.NewLogger()
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)

		// Get the flags from the command and also the additional flags specific to this command
		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		// Specify additional flags that are specific to this command
		additionalFlags := []flags.Flag{
			{Name: "filter-by-name", Type: "string"},
			{Name: "filter-by-tags", Type: "string"},
			{Name: "show-unhealthy", Type: "bool"},
			{Name: "show-tags", Type: "bool"},
		}
		flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
		if err != nil {
			logger.LogError("Error getting flags", err, nil, true)
			return err
		}

		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
			Profile:    aws.String((*flagValues)["profile"].(string)),
			Region:     aws.String((*flagValues)["region"].(string)),
		}

		cfg, err := handlers.NewConfig(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			logger.LogError("Failed loading AWS client config", err, nil, true)
			return err
		}

		client := elasticloadbalancing.NewFromConfig(*cfg)
		ec2Client := ec2.NewFromConfig(*cfg)

		collectDeletes := (*flagValues)["delete"].(bool)

		var wg sync.WaitGroup
		loadBalancerChan := make(chan types.LoadBalancerDescription, 100)
		tableRowChan := make(chan elbv1Result, 100)
		errorChan := make(chan error, 1)

		showUnhealthy := (*flagValues)["show-unhealthy"].(bool)
		showTags := (*flagValues)["show-tags"].(bool)
		filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))
		tagFilters := parseTagFilters((*flagValues)["filter-by-tags"].(string))

		headers := []string{"LoadBalancer Name", "number of listeners", "targets without instances"}
		if showUnhealthy {
			headers = append(headers, "targets unhealthy")
		}
		headers = append(headers, "VPC ID")
		if showTags {
			headers = append(headers, "Tags")
		}
		stream := printer.NewStreamTable(os.Stdout, true, headers)
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
		defer stream.Close()

		wg.Go(func() {
			if err := describeLoadBalancers(ctx, client, loadBalancerChan); err != nil {
				errorChan <- err
				close(loadBalancerChan)
				return
			}
			close(loadBalancerChan)
		})

		// Start a goroutine to process load balancers
		for lb := range loadBalancerChan {
			wg.Go(func() {
				if !matchesFilterValue(aws.ToString(lb.LoadBalancerName), filterByName) {
					return
				}
				tableRow, deleteName, err := handleLoadBalancer(ctx, &lb, client, ec2Client, showUnhealthy, showTags, tagFilters)
				if err != nil {
					errorChan <- err
					return
				}
				if tableRow != nil {
					tableRowChan <- elbv1Result{row: tableRow, deleteName: deleteName}
				}
			})
		}

		doneChan := make(chan struct{})
		go func() {
			wg.Wait()
			close(doneChan)
		}()

		deleteNames := make([]string, 0)
		for {
			select {
			case err := <-errorChan:
				logger.LogError("Error during loadbalancer processing", err, nil, true)
				return err
			case res := <-tableRowChan:
				if res.row != nil && len(*res.row) > 0 {
					stream.WriteRow((*res.row)...)
				}
				if collectDeletes && res.deleteName != "" {
					deleteNames = append(deleteNames, res.deleteName)
				}
			case <-doneChan:
				close(tableRowChan)
				for res := range tableRowChan {
					if res.row == nil || len(*res.row) == 0 {
						continue
					}
					stream.WriteRow((*res.row)...)
					if collectDeletes && res.deleteName != "" {
						deleteNames = append(deleteNames, res.deleteName)
					}
				}
				if !collectDeletes || len(deleteNames) == 0 {
					return nil
				}
				confirm, err := confirmDelete(prompterClient, logger)
				if err != nil {
					return err
				}
				if !confirm {
					return nil
				}
				for _, lbName := range deleteNames {
					logger.LogInfo("Deleting LoadBalancer", map[string]any{"LoadBalancerName": lbName})
					if _, err := client.DeleteLoadBalancer(ctx, &elasticloadbalancing.DeleteLoadBalancerInput{
						LoadBalancerName: aws.String(lbName),
					}); err != nil {
						return err
					}
				}
				return nil
			}
		}
	},
}

func init() {
	elbv1Cmd.Flags().String("filter-by-name", "", "Filter load balancers by name (supports * and ?).")
	elbv1Cmd.Flags().String("filter-by-tags", "", "Filter by tags (key=value or key; comma-separated).")
	elbv1Cmd.Flags().Bool("show-unhealthy", false, "Include load balancers with unhealthy targets.")
	elbv1Cmd.Flags().Bool("show-tags", false, "Include tags column in output.")
}

type elbv1Evaluation struct {
	noTargets           bool
	orphanTargets       []string
	unhealthyTargets    []string
	hasExistingTargets  bool
	hasUnhealthyTargets bool
}

type elbv1Result struct {
	row        *table.Row
	deleteName string
}

func handleLoadBalancer(ctx context.Context, elb *types.LoadBalancerDescription, client *elasticloadbalancing.Client, ec2Client *ec2.Client, showUnhealthy bool, showTags bool, tagFilters []tagFilter) (*table.Row, string, error) {
	needTags := showTags || len(tagFilters) > 0
	tagsValue := "-"
	if needTags {
		tagsMap, formattedTags, err := describeClassicElbTags(ctx, client, aws.ToString(elb.LoadBalancerName))
		if err != nil {
			return nil, "", err
		}
		if len(tagFilters) > 0 && !tagsMatchFilters(tagsMap, tagFilters) {
			return nil, "", nil
		}
		tagsValue = formattedTags
	}

	eval, err := evaluateElbv1(ctx, elb, client, ec2Client)
	if err != nil {
		return nil, "", err
	}
	if eval == nil {
		return nil, "", nil
	}

	isOrphan := eval.noTargets || (!eval.hasExistingTargets && len(eval.orphanTargets) > 0)
	shouldOutput := isOrphan || (showUnhealthy && eval.hasUnhealthyTargets)
	if !shouldOutput {
		return nil, "", nil
	}

	missingTargets := "-"
	if len(eval.orphanTargets) > 0 {
		missingTargets = strings.Join(eval.orphanTargets, "\n")
	}
	unhealthyTargets := "-"
	if len(eval.unhealthyTargets) > 0 {
		unhealthyTargets = strings.Join(eval.unhealthyTargets, "\n")
	}

	listenersCount := len(elb.ListenerDescriptions)
	vpcID := aws.ToString(elb.VPCId)
	if vpcID == "" {
		vpcID = "-"
	}

	row := table.Row{aws.ToString(elb.LoadBalancerName), listenersCount, missingTargets}
	if showUnhealthy {
		row = append(row, unhealthyTargets)
	}
	row = append(row, vpcID)
	if showTags {
		row = append(row, tagsValue)
	}

	deleteName := ""
	if isOrphan {
		name := aws.ToString(elb.LoadBalancerName)
		if name != "" {
			deleteName = name
		}
	}
	return &row, deleteName, nil
}

func describeLoadBalancers(ctx context.Context, client *elasticloadbalancing.Client, loadBalancerChan chan<- types.LoadBalancerDescription) error {
	paginator := elasticloadbalancing.NewDescribeLoadBalancersPaginator(client, &elasticloadbalancing.DescribeLoadBalancersInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, lb := range page.LoadBalancerDescriptions {
			loadBalancerChan <- lb
		}
	}
	return nil
}

func evaluateElbv1(ctx context.Context, elb *types.LoadBalancerDescription, client *elasticloadbalancing.Client, ec2Client *ec2.Client) (*elbv1Evaluation, error) {
	eval := &elbv1Evaluation{}
	if len(elb.Instances) == 0 {
		eval.noTargets = true
		return eval, nil
	}

	instanceHealth, err := client.DescribeInstanceHealth(ctx, &elasticloadbalancing.DescribeInstanceHealthInput{
		LoadBalancerName: elb.LoadBalancerName,
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

		exists, err := checkClassicInstanceExists(ctx, ec2Client, instanceID)
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

func checkClassicInstanceExists(ctx context.Context, ec2Client *ec2.Client, instanceID string) (bool, error) {
	if instanceID == "" {
		return false, nil
	}
	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
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

func describeClassicElbTags(ctx context.Context, client *elasticloadbalancing.Client, loadBalancerName string) (map[string]string, string, error) {
	if loadBalancerName == "" {
		return map[string]string{}, "-", nil
	}
	resp, err := client.DescribeTags(ctx, &elasticloadbalancing.DescribeTagsInput{
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
