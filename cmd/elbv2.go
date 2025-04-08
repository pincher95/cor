/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
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
	"github.com/jedib0t/go-pretty/v6/text"
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

		return runElbv2Cmd(ctx, &prompterClient, output, awsClient, flagValues)
	},
}

func init() {
	elbv2Cmd.Flags().String("filter-by-name", "", "The name of the elbv2 which matches an entire day.")
}

func runElbv2Cmd(ctx context.Context, prompter *prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	// Create an instance of elbv2Command
	elbCmd := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  *prompter,
		Output:    output,
	}

	return elbCmd.execute(ctx, flagValues)
}

func (e *AWSCommand) execute(ctx context.Context, flagValues *map[string]any) error {
	// Create channels to send load balancers
	loadBalancerChan := make(chan types.LoadBalancer, 50)
	resultsChan := make(chan table.Row, 50)

	// Create an errgroup with context
	g, ctx := errgroup.WithContext(ctx)

	// Goroutine to describe load balancers
	g.Go(func() error {
		if err := e.describeLoadBalancersV2(ctx, loadBalancerChan); err != nil {
			return err
		}
		return nil
	})

	numWorkers := NumGoroutines
	for range numWorkers {
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case lb, ok := <-loadBalancerChan:
					if !ok {
						return nil
					}
					tableRow, err := e.handleLoadBalancerV2(ctx, lb, (*flagValues)["filter-by-name"].(string))
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

	// Result collector goroutine: concurrently reads from resultsChan.
	resultCollectorDone := make(chan struct{})
	tableRows := make([]table.Row, 0)
	go func() {
		for res := range resultsChan {
			tableRows = append(tableRows, res)
		}
		close(resultCollectorDone)
	}()

	// Wait for the describer and workers to finish.
	if err := g.Wait(); err != nil {
		e.Logger.LogError("Error during volume processing", err, nil, false)
		return err
	}

	// All worker and describer goroutines are done; close the results channel.
	close(resultsChan)
	// Wait for the collector to finish.
	<-resultCollectorDone

	// Print table
	if err := printLoadBalancerV2Table(&tableRows); err != nil {
		e.Logger.LogError("Error printing table", err, nil, false)
		return err
	}

	if (*flagValues)["delete"].(bool) && len(tableRows) > 0 {
		confirm, err := e.Prompter.Confirm("Are you sure you want to proceed? (yes/no): ")
		if err != nil {
			e.Logger.LogError("Error during user prompt", err, nil, false)
			return err
		}

		if confirm == nil {
			e.Logger.LogInfo("Invalid response. Please enter 'yes' or 'no'.", nil)
		} else if *confirm {
			for _, tableRow := range tableRows {
				e.Logger.LogInfo("Deleting LoadBalancer", map[string]any{"LoadBalancerName": tableRow[0].(string)})

				// Delete Listeners
				if err := e.deleteListeners(ctx, aws.String(tableRow[1].(string))); err != nil {
					e.Logger.LogError("Error deleting listeners", err, nil, false)
					return err
				}

				// Delete Target Groups
				if err := e.deleteTargetGroups(ctx, strings.Split(tableRow[2].(string), "\n")); err != nil {
					e.Logger.LogError("Error deleting target groups", err, nil, false)
					return err
				}

				// Delete Load Balancer
				_, err = e.AWSClient.DeleteLoadBalancer(ctx, &elasticloadbalancingv2.DeleteLoadBalancerInput{
					LoadBalancerArn: aws.String(tableRow[1].(string)),
				})
				if err != nil {
					e.Logger.LogError("Error deleting loadbalancer", err, nil, false)
					return err
				}
			}
		} else if !*confirm {
			e.Logger.LogInfo("Aborted.", nil)
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
			e.Logger.LogInfo("Deleting listener", map[string]any{"ListenerArn": *listener.ListenerArn})
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
		e.Logger.LogInfo("Deleting target group", map[string]any{"TargetGroupName": *target.TargetGroupName})
		_, err = e.AWSClient.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{
			TargetGroupArn: target.TargetGroupArn,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func printLoadBalancerV2Table(tableRows *[]table.Row) error {

	columnConfig := []table.ColumnConfig{
		{
			Name:        "LoadBalancer Name",
			AlignHeader: text.AlignCenter,
		},
		{
			Name:        "LoadBalancer ARN",
			AlignHeader: text.AlignCenter,
		},
		{
			Name:        "targetGroups without targets",
			AlignHeader: text.AlignCenter,
		},
	}

	printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"LoadBalancer Name", "LoadBalancer ARN", "targerGroups without targets"}, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}}, &columnConfig)

	return printerClient.PrintTextTable(tableRows)
}

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
