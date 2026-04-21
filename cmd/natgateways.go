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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// natgatewaysCmd represents the natgateways command
var natgatewaysCmd = &cobra.Command{
	Use:   "natgateways",
	Short: "List and optionally delete NAT Gateways",
	Long:  `Find NAT Gateways that might be unused. NAT Gateways are billed hourly and for data processing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-state", Type: "string"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeNatGateways)
	},
}

func init() {
	natgatewaysCmd.Flags().String("filter-by-state", "available", "Filter NAT Gateways by state (available, deleted, deleting, failed, pending)")
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
	// Preserve the original context for delete operations (avoid errgroup ctx cancellation).
	rootCtx := ctx

	collectDeletes := (*flagValues)["delete"].(bool)

	natChan := make(chan types.NatGateway, 50)
	infoChan := make(chan natGatewayInfo, 50)

	g, egCtx := errgroup.WithContext(ctx)

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
			page, err := paginator.NextPage(egCtx)
			if err != nil {
				return err
			}
			for _, ng := range page.NatGateways {
				select {
				case <-egCtx.Done():
					return egCtx.Err()
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
				case <-egCtx.Done():
					return egCtx.Err()
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
	printDone := make(chan []natGatewayInfo, 1)
	go func() {
		stream := printer.NewStreamTable(c.Output, true, []string{"Name", "ID", "State", "VPC", "Subnet", "Created"})
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
		deleteCandidates := make([]natGatewayInfo, 0)
		finish := func() {
			stream.Close()
			printDone <- deleteCandidates
		}

		for info := range infoChan {
			stream.WriteRow(info.Name, info.ID, info.State, info.VpcID, info.Subnet, info.Created)

			if collectDeletes {
				deleteCandidates = append(deleteCandidates, info)
			}
		}
		finish()
	}()

	if err := g.Wait(); err != nil {
		c.Logger.LogError("Error processing NAT Gateways", err, nil, false)
		close(infoChan)
		<-printDone
		return err
	}
	close(infoChan)
	deleteCandidates := <-printDone
	if collectDeletes {
		if len(deleteCandidates) == 0 {
			return nil
		}
		confirm, err := confirmDelete(c.Prompter, c.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, info := range deleteCandidates {
			c.Logger.LogInfo("Deleting NAT Gateway", map[string]any{"ID": info.ID, "Name": info.Name})
			if _, err := c.AWSClient.DeleteNatGateway(rootCtx, &ec2.DeleteNatGatewayInput{NatGatewayId: aws.String(info.ID)}); err != nil {
				c.Logger.LogError("Error deleting NAT Gateway", err, map[string]any{"ID": info.ID}, false)
				return err
			}
		}
	}

	return nil
}
