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
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing/types"
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
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
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		// Create a new logger and error handler
		logger := logging.NewLogger()

		// Get the flags from the command and also the additional flags specific to this command
		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		// Specify additional flags that are specific to this command
		additionalFlags := []flags.Flag{}
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

		var wg sync.WaitGroup
		loadBalancerChan := make(chan types.LoadBalancerDescription, 100)
		tableRowChan := make(chan *table.Row, 100)
		errorChan := make(chan error, 1)

		stream := printer.NewStreamTable(os.Stdout, true, []string{"LoadBalancer Name", "number of listeners", "instance unhealthy", "VPC ID"})
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
		defer stream.Close()

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := describeLoadBalancers(ctx, client, loadBalancerChan); err != nil {
				errorChan <- err
				close(loadBalancerChan)
				return
			}
			close(loadBalancerChan)
		}()

		// Start a goroutine to process load balancers
		for lb := range loadBalancerChan {
			lb := lb
			wg.Add(1)
			go func() {
				defer wg.Done()
				tableRow, err := handleLoadBalancer(ctx, &lb, client)
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
				logger.LogError("Error during loadbalancer processing", err, nil, true)
				return err
			case row := <-tableRowChan:
				if row != nil {
					if len(*row) > 0 {
						stream.WriteRow((*row)...)
					}
				}
			case <-doneChan:
				close(tableRowChan)
				for row := range tableRowChan {
					if row == nil || len(*row) == 0 {
						continue
					}
					stream.WriteRow((*row)...)
				}
				return nil
			}
		}
	},
}

func init() {
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
