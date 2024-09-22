/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/promter"
	"github.com/spf13/cobra"
)

// elbv2Cmd represents the elbv2 command
var elbv2Cmd = &cobra.Command{
	Use:   "elbv2",
	Short: "Return Elastic LoadBalancer of type Application/Network",
	Long:  ``,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Create prompter using the prompter package
		prompterClient := promter.NewConsolePrompter(os.Stdin, os.Stdout)
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
		cfg, err := handlers.NewConfigV2(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			return err
		}

		// Create an instance of ELBV2 client
		elbClient := elasticloadbalancingv2.NewFromConfig(*cfg)

		awsClient := &handlers.AWSClientImpl{
			ELB: elbClient,
		}

		return runElbv2Cmd(ctx, &prompterClient, output, awsClient, flagValues)
	},
}

func init() {
	// rootCmd.AddCommand(elbv2Cmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// elbCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// elbCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
	elbv2Cmd.Flags().String("filter-by-name", "", "The name of the elbv2 which matches an entire day.")
}

func runElbv2Cmd(ctx context.Context, prompter *promter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]interface{}) error {
	// Create an instance of elbv2Command
	elbCmd := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  *prompter,
		Output:    output,
	}

	return elbCmd.execute(ctx, flagValues)
}

func (e *AWSCommand) execute(ctx context.Context, flagValues *map[string]interface{}) error {
	// Create channels to send load balancers
	loadBalancerChan := make(chan types.LoadBalancer, 50)
	tableRowChan := make(chan *table.Row, 50)
	errorChan := make(chan error, 1)
	doneChan := make(chan struct{})

	// Create a wait group
	var wg sync.WaitGroup

	// Create a slice of table.Row
	var tableRows []table.Row

	// Start a goroutine to describe load balancers
	go func() {
		if err := e.describeLoadBalancersV2(ctx, loadBalancerChan); err != nil {
			errorChan <- err
		}
	}()

	// Start a goroutine to process load balancers
	for lb := range loadBalancerChan {
		wg.Add(1)
		lb := lb
		go func(lb types.LoadBalancer) {
			defer wg.Done()
			fmt.Println("Processing LoadBalancer", *lb.LoadBalancerName)
			tableRow, err := e.handleLoadBalancerV2(ctx, lb, (*flagValues)["filter-by-name"].(string))
			if err != nil {
				errorChan <- err
				return
			}
			if tableRow != nil {
				tableRowChan <- tableRow
			}
		}(lb)
	}

	// Start a goroutine to wait for all processing to complete
	go func() {
		wg.Wait()
		close(doneChan)
	}()

	// Process the load balancers concurrently
	for {
		select {
		case err := <-errorChan:
			e.Logger.LogError("Error during loadbalancer processing", err, nil, false)
			return err
		case <-doneChan:
			close(tableRowChan)
			for row := range tableRowChan {
				tableRows = append(tableRows, *row)
			}

			// Print table
			if err := printLoadBalancerV2Table(&tableRows); err != nil {
				e.Logger.LogError("Error printing table", err, nil, false)
				errorChan <- err
			}

			if (*flagValues)["delete"].(bool) && len(tableRows) > 0 {
				confirm, err := e.Prompter.Confirm("Are you sure you want to proceed? (yes/no): ")
				if err != nil {
					e.Logger.LogError("Error during user prompt", err, nil, false)
					errorChan <- err
				}

				if confirm == nil {
					e.Logger.LogInfo("Invalid response. Please enter 'yes' or 'no'.", nil)
				} else if *confirm {
					for _, tableRow := range tableRows {
						e.Logger.LogInfo("Deleting LoadBalancer", map[string]interface{}{"LoadBalancerName": tableRow[0].(string)})

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
							errorChan <- err
						}
					}
				} else if !*confirm {
					e.Logger.LogInfo("Aborted.", nil)
				}
			}
			return nil
		case <-ctx.Done():
			e.Logger.LogInfo("Operation canceled", nil)
			return ctx.Err()
		}
	}
}

func (e *AWSCommand) describeLoadBalancersV2(ctx context.Context, loadBalancerChan chan<- types.LoadBalancer) error {
	defer close(loadBalancerChan)

	paginator := elasticloadbalancingv2.NewDescribeLoadBalancersPaginator(e.AWSClient.ELB, &elasticloadbalancingv2.DescribeLoadBalancersInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, lb := range page.LoadBalancers {
			fmt.Println("Found LoadBalancer", *lb.LoadBalancerName)
			loadBalancerChan <- lb
		}
	}
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

func (e *AWSCommand) processLoadBalancer(ctx context.Context, elb types.LoadBalancer, targetGroups []types.TargetGroup) (*table.Row, error) {
	var targetGroupsWithoutTargets []string
	var hasTargets bool

	for _, tg := range targetGroups {
		targetHealthDescriptions, err := e.getTargetHealthDescriptions(ctx, tg.TargetGroupArn)
		if err != nil {
			return nil, err
		}

		if len(targetHealthDescriptions) > 0 {
			hasTargets = true
		} else {
			targetGroupsWithoutTargets = append(targetGroupsWithoutTargets, *tg.TargetGroupName)
		}
	}

	if hasTargets {
		return nil, nil
	}

	if len(targetGroupsWithoutTargets) > 0 {
		return &table.Row{
			*elb.LoadBalancerName,
			*elb.LoadBalancerArn,
			strings.Join(targetGroupsWithoutTargets, "\n"),
			*elb.VpcId,
		}, nil
	}

	return nil, nil
}

func (e *AWSCommand) getTargetHealthDescriptions(ctx context.Context, targetGroupArn *string) ([]types.TargetHealthDescription, error) {
	output, err := e.AWSClient.DescribeTargetHealth(ctx, &elasticloadbalancingv2.DescribeTargetHealthInput{
		TargetGroupArn: targetGroupArn,
	})
	if err != nil {
		return nil, err
	}
	return output.TargetHealthDescriptions, nil
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
			e.Logger.LogInfo("Deleting listener", map[string]interface{}{"ListenerArn": *listener.ListenerArn})
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
		e.Logger.LogInfo("Deleting target group", map[string]interface{}{"TargetGroupName": *target.TargetGroupName})
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
			Name:        "targerGroups without targets",
			AlignHeader: text.AlignCenter,
		},
		{
			Name:        "VPC ID",
			AlignHeader: text.AlignCenter,
		},
	}

	printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"LoadBalancer Name", "LoadBalancer ARN", "targerGroups without targets", "VPC ID"}, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}}, &columnConfig)

	return printerClient.PrintTextTable(tableRows)
}
