/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing/types"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/pincher95/cor/pkg/handlers"
	"github.com/pincher95/cor/pkg/printer"
	"github.com/spf13/cobra"
)

// elbv1Cmd represents the elbv1 command
var elbv1Cmd = &cobra.Command{
	Use:   "elbv1",
	Short: "Return ELB of type Classic",
	Long:  `Return Classic ELB with instance state unhealthy.`,
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

		cfg, err := handlers.NewConfig(authMethod, profile, region, "UTC", true, true)
		if err != nil {
			panic(fmt.Sprintf("failed loading config, %v", err))
		}

		client := elasticloadbalancing.NewFromConfig(*cfg)

		var wg sync.WaitGroup

		loadBalancerChan := make(chan types.LoadBalancerDescription, 100)

		go func() {
			wg.Add(1)
			defer wg.Done()
			defer close(loadBalancerChan)
			if err := describeLoadBalancers(context.TODO(), client, loadBalancerChan); err != nil {
				fmt.Println(err)
			}
		}()

		go func() {
			wg.Wait()
		}()

		for elb := range loadBalancerChan {
			instance, err := client.DescribeInstanceHealth(context.TODO(), &elasticloadbalancing.DescribeInstanceHealthInput{
				LoadBalancerName: elb.LoadBalancerName,
			})
			if err != nil {
				fmt.Println(err)
			}

			for _, instanceState := range instance.InstanceStates {
				if *instanceState.State != "InService" {
					tableRows = append(tableRows, table.Row{*elb.LoadBalancerName, len(instance.InstanceStates), instanceState.InstanceId, *elb.VPCId})
				}
			}
		}

		columnConfig := []table.ColumnConfig{
			{Name: "LoadBalancer Name", AlignHeader: text.AlignCenter},
			{Name: "number of listeners", AlignHeader: text.AlignCenter},
			{Name: "instance unhealthy", AlignHeader: text.AlignCenter},
			{Name: "VPC ID", AlignHeader: text.AlignCenter},
		}

		printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"LoadBalancer Name", "number of listeners", "instance unhealthy", "VPC ID"}, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}}, &columnConfig)

		if err := printerClient.PrintTextTable(&tableRows); err != nil {
			fmt.Printf("%v", err)
		}
	},
}

func init() {
	// rootCmd.AddCommand(elbv1Cmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// elbv1Cmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// elbv1Cmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
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
