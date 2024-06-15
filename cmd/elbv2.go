/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/pincher95/cor/pkg/handlers"
	"github.com/pincher95/cor/pkg/printer"
	"github.com/spf13/cobra"
)

// elbv2Cmd represents the elbv2 command
var elbv2Cmd = &cobra.Command{
	Use:   "elbv2",
	Short: "Return Elastic LoadBalancer of type Application/Network",
	Long:  ``,
	Run: func(cmd *cobra.Command, args []string) {
		region, err := cmd.Flags().GetString("region")
		if err != nil {
			fmt.Println(err)
		}

		authMethod, err := cmd.Flags().GetString("auth-method")
		if err != nil {
			fmt.Println(err)
		}

		profile, err := cmd.Flags().GetString("profile")
		if err != nil {
			fmt.Println(err)
		}

		delete, err := cmd.Flags().GetBool("delete")
		if err != nil {
			fmt.Println(err)
		}

		elbName, err := cmd.Flags().GetString("filter-by-name")
		if err != nil {
			fmt.Println(err)
		}

		cfg, err := handlers.NewConfig(authMethod, profile, region, "UTC", true, true)
		if err != nil {
			panic(fmt.Sprintf("failed loading config, %v", err))
		}

		client := elasticloadbalancingv2.NewFromConfig(*cfg)

		var wg sync.WaitGroup

		loadBalancerChan := make(chan types.LoadBalancer, 100)

		go func() {
			wg.Add(1)
			defer wg.Done()
			defer close(loadBalancerChan)
			if err := describeLoadBalancersV2(context.TODO(), client, loadBalancerChan); err != nil {
				fmt.Println(err)
			}
		}()

		go func() {
			wg.Wait()
		}()

		for lb := range loadBalancerChan {
			if strings.Contains(*lb.LoadBalancerName, elbName) {
				targerGroups, err := client.DescribeTargetGroups(context.TODO(), &elasticloadbalancingv2.DescribeTargetGroupsInput{
					LoadBalancerArn: lb.LoadBalancerArn,
				})
				if err != nil {
					fmt.Println(err)
				}
				targetGroupNames := make([]string, 0, len(targerGroups.TargetGroups))
				for index, targerGroup := range targerGroups.TargetGroups {
					targets, err := client.DescribeTargetHealth(context.TODO(), &elasticloadbalancingv2.DescribeTargetHealthInput{
						TargetGroupArn: targerGroup.TargetGroupArn,
					})
					if err != nil {
						fmt.Println(err)
					}

					if len(targets.TargetHealthDescriptions) < 1 {
						targetGroupNames = append(targetGroupNames, *targerGroup.TargetGroupName)
					}

					if index == len(targerGroups.TargetGroups)-1 && len(targetGroupNames) >= 1 {
						tableRows = append(tableRows, table.Row{*lb.LoadBalancerName, *lb.LoadBalancerArn, len(targerGroups.TargetGroups), strings.Join(targetGroupNames, "\n"), *lb.VpcId})
						targetGroupNames = nil
					}
				}
			}
		}

		columnConfig := []table.ColumnConfig{
			{Name: "LoadBalancer Name", AlignHeader: text.AlignCenter},
			{Name: "LoadBalancer ARN", AlignHeader: text.AlignCenter},
			{Name: "number of targetgroups", AlignHeader: text.AlignCenter},
			{Name: "targerGroups without targets", AlignHeader: text.AlignCenter},
			{Name: "VPC ID", AlignHeader: text.AlignCenter},
		}

		printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"LoadBalancer Name", "LoadBalancer ARN", "number of targetgroups", "targerGroups without targets", "VPC ID"}, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}}, &columnConfig)

		if err := printerClient.PrintTextTable(&tableRows); err != nil {
			fmt.Printf("%v", err)
		}

		if delete {
			reader := bufio.NewReader(os.Stdin)
			fmt.Print("Are you sure you want to proceed? (yes/no): ")
			response, _ := reader.ReadString('\n')
			response = strings.ToLower(strings.TrimSpace(response))

			if response == "yes" {
				for _, tableRow := range tableRows {
					if tableRow[2].(int) > len(tableRow[3].([]string)) {
						fmt.Println("Cannot delete LoadBalancer containing active targets")
						continue
					}

					targerGroups, err := client.DescribeTargetGroups(context.TODO(), &elasticloadbalancingv2.DescribeTargetGroupsInput{
						Names: tableRow[3].([]string),
					})
					if err != nil {
						fmt.Println(err)
					}

					_, err = client.DeleteLoadBalancer(context.TODO(), &elasticloadbalancingv2.DeleteLoadBalancerInput{
						LoadBalancerArn: aws.String(tableRow[1].(string)),
					})
					if err != nil {
						fmt.Println(err)
					}

					for _, target := range targerGroups.TargetGroups {
						_, err = client.DeleteTargetGroup(context.TODO(), &elasticloadbalancingv2.DeleteTargetGroupInput{
							TargetGroupArn: target.TargetGroupArn,
						})
						if err != nil {
							fmt.Println(err)
						}
					}

					_, err = client.DeleteLoadBalancer(context.TODO(), &elasticloadbalancingv2.DeleteLoadBalancerInput{
						LoadBalancerArn: aws.String(tableRow[1].(string)),
					})
					if err != nil {
						fmt.Println(err)
					}
				}
			} else if response == "no" {
				fmt.Println("Aborted.")
				// Your code to handle aborting here
			} else {
				fmt.Println("Invalid response. Please enter 'yes' or 'no'.")
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
	paginator := elasticloadbalancingv2.NewDescribeLoadBalancersPaginator(client, &elasticloadbalancingv2.DescribeLoadBalancersInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, lb := range page.LoadBalancers {
			loadBalancerChan <- lb
		}
	}
	return nil
}
