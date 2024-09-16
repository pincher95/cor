/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing/types"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/errorhandling"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
)

// elbv1Cmd represents the elbv1 command
var elbv1Cmd = &cobra.Command{
	Use:   "elbv1",
	Short: "Return ELB of type Classic",
	Long:  `Return Classic ELB with instance state unhealthy.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.TODO()

		// Create a new logger and error handler
		logger := logging.NewLogger()
		errorHandler := errorhandling.NewErrorHandler(logger)

		// Get the flags from the command and also the additional flags specific to this command
		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		// Specify additional flags that are specific to this command
		additionalFlags := []flags.Flag{}
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

		client := elasticloadbalancing.NewFromConfig(*cfg)

		var wg sync.WaitGroup
		loadBalancerChan := make(chan types.LoadBalancerDescription, 100)
		tableRowChan := make(chan *table.Row, 100)
		errorChan := make(chan error, 1)

		// Create a slice of table.Row
		var tableRows []table.Row

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := describeLoadBalancers(context.TODO(), client, loadBalancerChan); err != nil {
				errorChan <- err
				close(loadBalancerChan)
				return
			}
			close(loadBalancerChan)
		}()

		// Start a goroutine to process load balancers
		for lb := range loadBalancerChan {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tableRow, err := handleLoadBalancer(context.TODO(), &lb, client)
				if err != nil {
					errorChan <- err
					return
				}
				if tableRow != nil {
					tableRowChan <- tableRow
				}
			}()
		}

		doneChan := make(chan struct{})
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
						Name:        "number of listeners",
						AlignHeader: text.AlignCenter,
					},
					{
						Name:        "instance unhealthy",
						AlignHeader: text.AlignCenter,
					},
					{
						Name:        "VPC ID",
						AlignHeader: text.AlignCenter,
					},
				}

				printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"LoadBalancer Name", "number of listeners", "instance unhealthy", "VPC ID"}, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}}, &columnConfig)

				if err := printerClient.PrintTextTable(&tableRows); err != nil {
					errorHandler.HandleError("Error printing table", err, nil, false)
				}
				return
			}
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

func handleLoadBalancer(ctx context.Context, elb *types.LoadBalancerDescription, client *elasticloadbalancing.Client) (*table.Row, error) {
	instance, err := client.DescribeInstanceHealth(ctx, &elasticloadbalancing.DescribeInstanceHealthInput{
		LoadBalancerName: elb.LoadBalancerName,
	})
	if err != nil {
		return nil, err
	}

	for _, instanceState := range instance.InstanceStates {
		if *instanceState.State != "InService" {
			return &table.Row{*elb.LoadBalancerName, len(instance.InstanceStates), *instanceState.InstanceId, *elb.VPCId}, nil
		}
	}
	return nil, nil
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
