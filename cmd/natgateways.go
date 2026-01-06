/*
Copyright 2024 Elastic Scaler Contributors.

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
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// natgatewaysCmd represents the natgateways command
var natgatewaysCmd = &cobra.Command{
	Use:   "natgateways",
	Short: "List and optionally delete NAT Gateways",
	Long:  `Find NAT Gateways that might be unused. NAT Gateways are billed hourly and for data processing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout
		ctx := cmd.Context()

		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		additionalFlags := []flags.Flag{
			{Name: "filter-by-state", Type: "string"},
		}

		flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
		if err != nil {
			return err
		}

		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
			Profile:    aws.String((*flagValues)["profile"].(string)),
			Region:     aws.String((*flagValues)["region"].(string)),
		}

		cfg, err := handlers.NewConfig(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			return err
		}

		ec2Client := ec2.NewFromConfig(*cfg)

		awsClient := &handlers.AWSClientImpl{
			EC2: ec2Client,
		}

		return runNatGatewaysCmd(ctx, prompterClient, output, awsClient, flagValues)
	},
}

func init() {
	natgatewaysCmd.Flags().String("filter-by-state", "available", "Filter NAT Gateways by state (available, deleted, deleting, failed, pending)")
}

func runNatGatewaysCmd(ctx context.Context, prompter prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  prompter,
		Output:    output,
	}
	return command.executeNatGateways(ctx, flagValues)
}

type natGatewayInfo struct {
	Name    string
	ID      string
	State   string
	VpcID   string
	Subnet  string
	Created string
}

func (c *AWSCommand) executeNatGateways(ctx context.Context, flagValues *map[string]any) error {
	// If deleting, confirm up-front so we can stream without buffering IDs.
	doDelete := false
	if (*flagValues)["delete"].(bool) {
		confirm, err := c.Prompter.Confirm("Are you sure you want to proceed? (yes/no): ")
		if err != nil {
			return err
		}
		if confirm == nil || !*confirm {
			c.Logger.LogInfo("Aborted.", nil)
			return nil
		}
		doDelete = true
	}

	natChan := make(chan types.NatGateway, 50)
	infoChan := make(chan natGatewayInfo, 50)

	g, ctx := errgroup.WithContext(ctx)

	// Producer
	g.Go(func() error {
		defer close(natChan)
		stateFilter := (*flagValues)["filter-by-state"].(string)
		filters := []types.Filter{}
		if stateFilter != "" {
			filters = append(filters, types.Filter{
				Name:   aws.String("state"),
				Values: []string{stateFilter},
			})
		}

		paginator := ec2.NewDescribeNatGatewaysPaginator(c.AWSClient.EC2, &ec2.DescribeNatGatewaysInput{
			Filter: filters,
		})

		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return err
			}
			for _, ng := range page.NatGateways {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case natChan <- ng:
				}
			}
		}
		return nil
	})

	// Workers
	for range NumGoroutines {
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case ng, ok := <-natChan:
					if !ok {
						return nil
					}

					name := "-"
					for _, tag := range ng.Tags {
						if tag.Key != nil && *tag.Key == "Name" && tag.Value != nil {
							name = *tag.Value
							break
						}
					}

					infoChan <- natGatewayInfo{
						Name:    name,
						ID:      aws.ToString(ng.NatGatewayId),
						State:   string(ng.State),
						VpcID:   aws.ToString(ng.VpcId),
						Subnet:  aws.ToString(ng.SubnetId),
						Created: ng.CreateTime.String(),
					}
				}
			}
		})
	}

	// Printer (stream)
	printDone := make(chan error, 1)
	go func() {
		stream := printer.NewStreamTable(c.Output, true, []string{"Name", "ID", "State", "VPC", "Subnet", "Created"})
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
		finish := func(err error) {
			stream.Close()
			printDone <- err
		}

		for info := range infoChan {
			stream.WriteRow(info.Name, info.ID, info.State, info.VpcID, info.Subnet, info.Created)

			if doDelete {
				c.Logger.LogInfo("Deleting NAT Gateway", map[string]any{"ID": info.ID, "Name": info.Name})
				if _, err := c.AWSClient.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{NatGatewayId: aws.String(info.ID)}); err != nil {
					finish(err)
					return
				}
			}
		}
		finish(nil)
	}()

	if err := g.Wait(); err != nil {
		c.Logger.LogError("Error processing NAT Gateways", err, nil, false)
		close(infoChan)
		<-printDone
		return err
	}
	close(infoChan)
	if err := <-printDone; err != nil {
		c.Logger.LogError("Error streaming/deleting NAT Gateways", err, nil, false)
		return err
	}

	return nil
}
