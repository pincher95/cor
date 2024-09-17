/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
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

type ELBV2Client interface {
	DescribeTargetGroups(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetGroupsInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetGroupsOutput, error)
	DescribeTargetHealth(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetHealthInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetHealthOutput, error)
	// Add other methods as needed
}

// elbv2Cmd represents the elbv2 command
var elbv2Cmd = &cobra.Command{
	Use:   "elbv2",
	Short: "Return Elastic LoadBalancer of type Application/Network",
	Long:  ``,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		// Create a new logger and error handler
		logger := logging.NewLogger()

		// Create channels to send load balancers
		loadBalancerChan := make(chan types.LoadBalancer, 50)
		tableRowChan := make(chan *table.Row, 50)
		errorChan := make(chan error, 1)
		doneChan := make(chan struct{})

		// Get the flags from the command and also the additional flags specific to this command
		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		// Specify additional flags that are specific to this command
		additionalFlags := []flags.Flag{
			{
				Name: "filter-by-name",
				Type: "string",
			},
			{
				Name: "delete",
				Type: "bool",
			},
		}
		flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
		if err != nil {
			logger.LogError("Error getting flags", err, nil, true)
			errorChan <- err
		}

		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String(flagValues["auth-method"].(string)),
			Profile:    aws.String(flagValues["profile"].(string)),
			Region:     aws.String(flagValues["region"].(string)),
		}

		// Create a new AWS client
		cfg, err := handlers.NewConfigV2(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			logger.LogError("Failed loading AWS client config", err, nil, true)
			errorChan <- err
		}

		// Create a new ELBv2 client
		client := elasticloadbalancingv2.NewFromConfig(*cfg)

		// Create a wait group
		var wg sync.WaitGroup

		// Create a slice of table.Row
		var tableRows []table.Row

		// Start a goroutine to describe load balancers
		go func() {
			if err := describeLoadBalancersV2(ctx, client, loadBalancerChan); err != nil {
				errorChan <- err
			}
		}()

		// Start a goroutine to process load balancers
		for lb := range loadBalancerChan {
			wg.Add(1)
			lb := lb
			go func(lb types.LoadBalancer) {
				defer wg.Done()
				tableRow, err := handleLoadBalancerV2(ctx, client, lb, flagValues["filter-by-name"].(string))
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

		for {
			select {
			case err := <-errorChan:
				logger.LogError("Error during loadbalancer processing", err, nil, true)
				return err
			case <-doneChan:
				close(tableRowChan)
				for row := range tableRowChan {
					tableRows = append(tableRows, *row)
				}

				// Print table
				if err := printLoadBalancerV2Table(&tableRows); err != nil {
					logger.LogError("Error printing table", err, nil, true)
					errorChan <- err
				}

				if flagValues["delete"].(bool) && len(tableRows) > 0 {
					userPromter := promter.NewConsolePrompter(os.Stdin, os.Stdout)

					confirm, err := userPromter.Confirm("Are you sure you want to proceed? (yes/no): ")
					if err != nil {
						logger.LogError("Error during user prompt", err, nil, false)
						errorChan <- err
					}

					if confirm == nil {
						logger.LogInfo("Invalid response. Please enter 'yes' or 'no'.", nil)
					} else if *confirm {
						for _, tableRow := range tableRows {
							logger.LogInfo("Deleting LoadBalancer", map[string]interface{}{"LoadBalancerName": tableRow[0].(string)})

							listenerPaginator := elasticloadbalancingv2.NewDescribeListenersPaginator(client, &elasticloadbalancingv2.DescribeListenersInput{
								LoadBalancerArn: aws.String(tableRow[1].(string)),
							})

							for listenerPaginator.HasMorePages() {
								listenerPage, err := listenerPaginator.NextPage(ctx)
								if err != nil {
									logger.LogError("Error during loadbalancer listerner processing", err, nil, false)
									errorChan <- err
								}

								for _, listener := range listenerPage.Listeners {
									logger.LogInfo("Deleting target group", map[string]interface{}{"listenerArn": *listener.ListenerArn})
									// Delete the listener
									_, err := client.DeleteListener(ctx, &elasticloadbalancingv2.DeleteListenerInput{
										ListenerArn: listener.ListenerArn,
									})
									if err != nil {
										logger.LogError("Error deleting loadbalancer listerner", err, nil, false)
										errorChan <- err
									}
								}
							}

							targerGroups, err := client.DescribeTargetGroups(ctx, &elasticloadbalancingv2.DescribeTargetGroupsInput{
								Names: strings.Split(tableRow[2].(string), "\n"),
							})
							if err != nil {
								logger.LogError("Error during targets groups processing", err, nil, false)
								errorChan <- err
							}

							for _, target := range targerGroups.TargetGroups {
								logger.LogInfo("Deleting target group", map[string]interface{}{"targetGroupName": *target.TargetGroupName})
								_, err = client.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{
									TargetGroupArn: target.TargetGroupArn,
								})
								if err != nil {
									logger.LogError("Error deleting targets groups", err, nil, false)
									errorChan <- err
								}
							}

							_, err = client.DeleteLoadBalancer(ctx, &elasticloadbalancingv2.DeleteLoadBalancerInput{
								LoadBalancerArn: aws.String(tableRow[1].(string)),
							})
							if err != nil {
								logger.LogError("Error deleting loadbalancer", err, nil, false)
								errorChan <- err
							}
						}
					} else if !*confirm {
						logger.LogInfo("Aborted.", nil)
					}
				}
				return nil
			case <-ctx.Done():
				logger.LogInfo("Operation canceled", nil)
				return ctx.Err()
			}
		}
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

func describeLoadBalancersV2(ctx context.Context, client *elasticloadbalancingv2.Client, loadBalancerChan chan<- types.LoadBalancer) error {
	defer close(loadBalancerChan)

	paginator := elasticloadbalancingv2.NewDescribeLoadBalancersPaginator(client, &elasticloadbalancingv2.DescribeLoadBalancersInput{
		// PageSize: aws.Int32(100),
	})
	for paginator.HasMorePages() {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Proceed to get the next page
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return err
			}
			for _, lb := range page.LoadBalancers {
				select {
				case loadBalancerChan <- lb:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
	return nil
}

func handleLoadBalancerV2(ctx context.Context, client ELBV2Client, elb types.LoadBalancer, filterByName string) (*table.Row, error) {
	// Early exit if the load balancer name doesn't match the filter
	if filterByName != "" && !strings.Contains(*elb.LoadBalancerName, filterByName) {
		return nil, nil
	}

	// Get target groups for the load balancer
	targetGroups, err := getTargetGroups(ctx, client, elb.LoadBalancerArn)
	if err != nil {
		return nil, err
	}

	// Process the load balancer and its target groups
	row, err := processLoadBalancer(ctx, client, elb, targetGroups)
	if err != nil {
		return nil, err
	}

	return row, nil
}

func getTargetGroups(ctx context.Context, client ELBV2Client, loadBalancerArn *string) ([]types.TargetGroup, error) {
	var targetGroups []types.TargetGroup
	paginator := elasticloadbalancingv2.NewDescribeTargetGroupsPaginator(client, &elasticloadbalancingv2.DescribeTargetGroupsInput{
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

func processLoadBalancer(ctx context.Context, client ELBV2Client, elb types.LoadBalancer, targetGroups []types.TargetGroup) (*table.Row, error) {
	var targetGroupsWithoutTargets []string
	var hasTargets bool

	for _, tg := range targetGroups {
		targetHealthDescriptions, err := getTargetHealthDescriptions(ctx, client, tg.TargetGroupArn)
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

func getTargetHealthDescriptions(ctx context.Context, client ELBV2Client, targetGroupArn *string) ([]types.TargetHealthDescription, error) {
	output, err := client.DescribeTargetHealth(ctx, &elasticloadbalancingv2.DescribeTargetHealthInput{
		TargetGroupArn: targetGroupArn,
	})
	if err != nil {
		return nil, err
	}
	return output.TargetHealthDescriptions, nil
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
