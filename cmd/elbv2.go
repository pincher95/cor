/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"bufio"
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
	"github.com/pincher95/cor/pkg/handlers/errorhandling"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
)

// elbv2Cmd represents the elbv2 command
var elbv2Cmd = &cobra.Command{
	Use:   "elbv2",
	Short: "Return Elastic LoadBalancer of type Application/Network",
	Long:  ``,
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.TODO()

		// Create a new logger and error handler
		logger := logging.NewLogger()
		errorHandler := errorhandling.NewErrorHandler(logger)

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
			errorHandler.HandleError("Error getting flags", err, nil, true)
			return
		}

		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String(flagValues["auth-method"].(string)),
			Profile:    aws.String(flagValues["profile"].(string)),
			Region:     aws.String(flagValues["region"].(string)),
		}

		cfg, err := handlers.NewConfigV2(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			errorHandler.HandleError("Failed loading AWS client config", err, nil, true)
			return
		}

		client := elasticloadbalancingv2.NewFromConfig(*cfg)

		var wg sync.WaitGroup
		loadBalancerChan := make(chan types.LoadBalancer, 50)
		tableRowChan := make(chan *table.Row, 50)
		errorChan := make(chan error, 1)
		doneChan := make(chan struct{})

		// Create a slice of table.Row
		var tableRows []table.Row

		// Start a goroutine to describe load balancers
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := describeLoadBalancersV2(ctx, client, loadBalancerChan); err != nil {
				errorChan <- err
				return
			}
		}()

		// Start a goroutine to process load balancers
		for lb := range loadBalancerChan {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tableRow, err := handleLoadBalancerV2(ctx, lb, client, flagValues["filter-by-name"].(string))
				if err != nil {
					errorChan <- err
					return
				}
				if tableRow != nil {
					tableRowChan <- tableRow
				}
			}()
		}

		// Start a goroutine to wait for all processing to complete
		go func() {
			wg.Wait()
			close(doneChan)
		}()

		for {
			select {
			case err := <-errorChan:
				errorHandler.HandleError("Error during loadbalancer processing", err, nil, true)
				return
			case <-doneChan:
				close(tableRowChan)
				for row := range tableRowChan {
					tableRows = append(tableRows, *row)
				}
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

				if err := printerClient.PrintTextTable(&tableRows); err != nil {
					errorHandler.HandleError("Error printing table", err, nil, false)
				}

				if flagValues["delete"].(bool) {
					reader := bufio.NewReader(os.Stdin)
					logger.LogInfo("Are you sure you want to proceed? (yes/no): ", nil)
					response, _ := reader.ReadString('\n')
					response = strings.ToLower(strings.TrimSpace(response))

					if response == "yes" || response == "y" {
						for _, tableRow := range tableRows {
							logger.LogInfo("Deleting LoadBalancer", map[string]interface{}{"LoadBalancerName": tableRow[0].(string)})

							listenerPaginator := elasticloadbalancingv2.NewDescribeListenersPaginator(client, &elasticloadbalancingv2.DescribeListenersInput{
								LoadBalancerArn: aws.String(tableRow[1].(string)),
							})

							for listenerPaginator.HasMorePages() {
								listenerPage, err := listenerPaginator.NextPage(ctx)
								if err != nil {
									errorHandler.HandleError("Error during loadbalancer listerner processing", err, nil, false)
								}

								for _, listener := range listenerPage.Listeners {
									logger.LogInfo("Deleting target group", map[string]interface{}{"listenerArn": *listener.ListenerArn})
									// Delete the listener
									_, err := client.DeleteListener(ctx, &elasticloadbalancingv2.DeleteListenerInput{
										ListenerArn: listener.ListenerArn,
									})
									if err != nil {
										errorHandler.HandleError("Error deleting loadbalancer listerner", err, nil, false)
									}
								}
							}

							targerGroups, err := client.DescribeTargetGroups(context.TODO(), &elasticloadbalancingv2.DescribeTargetGroupsInput{
								Names: strings.Split(tableRow[2].(string), "\n"),
							})
							if err != nil {
								errorHandler.HandleError("Error during targets groups processing", err, nil, false)
							}

							for _, target := range targerGroups.TargetGroups {
								logger.LogInfo("Deleting target group", map[string]interface{}{"targetGroupName": *target.TargetGroupName})
								_, err = client.DeleteTargetGroup(context.TODO(), &elasticloadbalancingv2.DeleteTargetGroupInput{
									TargetGroupArn: target.TargetGroupArn,
								})
								if err != nil {
									errorHandler.HandleError("Error deleting targets groups", err, nil, false)
								}
							}

							_, err = client.DeleteLoadBalancer(context.TODO(), &elasticloadbalancingv2.DeleteLoadBalancerInput{
								LoadBalancerArn: aws.String(tableRow[1].(string)),
							})
							if err != nil {
								errorHandler.HandleError("Error deleting loadbalancer", err, nil, false)
							}
						}
					} else if response == "no" || response == "n" {
						logger.LogInfo("Aborted.", nil)
					} else {
						logger.LogInfo("Invalid response. Please enter 'yes' or 'no'.", nil)
					}
				}
				return
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
	paginator := elasticloadbalancingv2.NewDescribeLoadBalancersPaginator(client, &elasticloadbalancingv2.DescribeLoadBalancersInput{
		// PageSize: aws.Int32(100),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			close(loadBalancerChan)
			return err
		}
		for _, lb := range page.LoadBalancers {
			loadBalancerChan <- lb
		}
	}
	close(loadBalancerChan)
	return nil
}

func handleLoadBalancerV2(ctx context.Context, elb types.LoadBalancer, client *elasticloadbalancingv2.Client, filterByName string) (*table.Row, error) {
	// Filter by load balancer name, exit early if the name doesn't match the filter
	if filterByName != "" && !strings.Contains(*elb.LoadBalancerName, filterByName) {
		return nil, nil
	}

	// Paginator for target groups of the load balancer
	tgPaginator := elasticloadbalancingv2.NewDescribeTargetGroupsPaginator(client, &elasticloadbalancingv2.DescribeTargetGroupsInput{
		LoadBalancerArn: elb.LoadBalancerArn,
	})

	var targetGroupsWithoutTargets []string
	var hasTargets bool

	for tgPaginator.HasMorePages() {
		tgPage, err := tgPaginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, tg := range tgPage.TargetGroups {
			// Describe target health to see if the target group has any targets
			targetHealth, err := client.DescribeTargetHealth(ctx, &elasticloadbalancingv2.DescribeTargetHealthInput{
				TargetGroupArn: tg.TargetGroupArn,
			})
			if err != nil {
				return nil, err
			}

			// If any target group has targets, mark hasTargets as true
			if len(targetHealth.TargetHealthDescriptions) > 0 {
				hasTargets = true
			} else {
				// Collect empty target groups
				targetGroupsWithoutTargets = append(targetGroupsWithoutTargets, *tg.TargetGroupName)
			}
		}
	}

	// If there are any target groups with targets, return nil
	if hasTargets {
		return nil, nil
	}

	// If all target groups are empty, return the load balancer details
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
