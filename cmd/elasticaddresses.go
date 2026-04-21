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
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/utils"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

type addressWithTags struct {
	Address types.Address
	TagMap  map[string]types.Tag
}

// elasticaddressesCmd represents the elasticaddresses command
var elasticIPsCmd = &cobra.Command{
	Use:   "elasticips",
	Short: "List and optionally release unassociated Elastic IPs",
	Long:  `List Elastic IP addresses that are not associated to any instance/network interface and optionally release them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeElasticIPs)
	},
}

func init() {
	elasticIPsCmd.Flags().String("filter-by-name", "", "Filter Elastic IPs by tag:Name (empty = no filter).")
}

func (a *AWSCommand) executeElasticIPs(ctx context.Context, flagValues *map[string]any) error {
	// Preserve the original context for delete operations (avoid errgroup ctx cancellation).
	rootCtx := ctx

	collectDeletes := (*flagValues)["delete"].(bool)

	// Create a channel to process addresses
	addressChan := make(chan addressWithTags, 10)
	resultsChan := make(chan table.Row, 10)

	// Create an errgroup with context
	g, egCtx := errgroup.WithContext(ctx)

	// Goroutine to describe addresses
	filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))
	g.Go(func() error {
		elasticIPFilter := []types.Filter{}
		if filterByName != "" {
			elasticIPFilter = append(elasticIPFilter, types.Filter{
				Name:   aws.String("tag:Name"),
				Values: []string{filterByName},
			})
		}
		if err := a.describeAddresses(egCtx, addressChan, &elasticIPFilter); err != nil {
			return err
		}
		return nil
	})

	// Launch worker goroutines
	numWorkers := NumGoroutines
	for range numWorkers {
		g.Go(func() error {
			for {
				select {
				case <-egCtx.Done():
					return nil
				case addressWithTags, ok := <-addressChan:
					if !ok {
						return nil
					}
					// processAddressWithTags, err := handleElasticIP(addressWithTags)
					// if err != nil {
					// 	return err
					// }

					address := addressWithTags.Address

					if address.AssociationId == nil {
						if address.InstanceId == nil {
							// Safely dereference pointers with nil checks
							name := "-"
							if nameTag, ok := utils.TagsToMap(address.Tags)["Name"]; ok && nameTag.Value != nil {
								name = *nameTag.Value
							}

							associationId := "-"
							if address.AssociationId != nil {
								associationId = *address.AssociationId
							}

							elasticIP := "-"
							if address.PublicIp != nil {
								elasticIP = *address.PublicIp
							}

							allocationId := "-"
							if address.AllocationId != nil {
								allocationId = *address.AllocationId
							}

							networkInterfaceId := "-"
							if address.NetworkInterfaceId != nil {
								networkInterfaceId = *address.NetworkInterfaceId
							}
							// Send the row to the results channel
							resultsChan <- table.Row{name, allocationId, elasticIP, associationId, networkInterfaceId}
						}
					}
				}
			}
		})
	}

	// Printer goroutine: stream output as rows arrive.
	resultDone := make(chan []ec2.ReleaseAddressInput, 1)
	go func() {
		stream := printer.NewStreamTable(a.Output, true, []string{"Name", "Allocation ID", "Allocated Public address", "Association ID", "Network interface ID"})
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))

		deleteInputs := make([]ec2.ReleaseAddressInput, 0)
		for row := range resultsChan {
			stream.WriteRow(row...)

			if collectDeletes {
				// Expect: Name, AllocationID, PublicIP, AssociationID, NetworkInterfaceID
				if len(row) < 3 {
					continue
				}

				allocationID, _ := row[1].(string)
				publicIP, _ := row[2].(string)

				// Prefer AllocationId (VPC EIPs), fall back to PublicIp (EC2-Classic).
				input := ec2.ReleaseAddressInput{}
				if allocationID != "" && allocationID != "-" {
					input.AllocationId = aws.String(allocationID)
				} else if publicIP != "" && publicIP != "-" {
					input.PublicIp = aws.String(publicIP)
				} else {
					continue
				}
				deleteInputs = append(deleteInputs, input)
			}
		}
		stream.Close()
		resultDone <- deleteInputs
	}()

	// Wait for the describer and workers to finish.
	err := g.Wait()

	// All worker and describer goroutines are done; close the results channel.
	close(resultsChan)
	deleteInputs := <-resultDone
	if err != nil {
		a.Logger.LogError("Error during elastic IP processing", err, nil, false)
		return err
	}
	if collectDeletes {
		if len(deleteInputs) == 0 {
			return nil
		}
		confirm, err := confirmDelete(a.Prompter, a.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, input := range deleteInputs {
			allocationID := aws.ToString(input.AllocationId)
			publicIP := aws.ToString(input.PublicIp)
			a.Logger.LogInfo("Releasing Elastic IP", map[string]any{"AllocationId": allocationID, "PublicIp": publicIP})
			if _, err := a.AWSClient.EC2.ReleaseAddress(rootCtx, &input); err != nil {
				a.Logger.LogError("Error releasing Elastic IP", err, map[string]any{"AllocationId": allocationID, "PublicIp": publicIP}, false)
				return err
			}
		}
	}

	return nil
}

func (a *AWSCommand) describeAddresses(ctx context.Context, addressChan chan<- addressWithTags, filters *[]types.Filter) error {
	defer func() {
		if recover() != nil {
			// Prevent panic if the channel is already closed
			a.Logger.LogError("Channel `addressChan` closed", nil, nil, false)
		}
		close(addressChan)
	}()

	// If filters are nil, create an empty filter
	if filters == nil {
		filters = &[]types.Filter{}
	}

	// Describe the addresses
	output, err := a.AWSClient.EC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: *filters,
	})
	if err != nil {
		return err
	}

	// Send volumes to the channel
	for _, address := range output.Addresses {
		tagMap := utils.TagsToMap(address.Tags)
		addressChan <- addressWithTags{Address: address, TagMap: tagMap}
	}

	return nil
}

// func handleElasticIP(address addressWithTags) (*addressWithTags, error) {
// 	_, ok := address.TagMap["Name"]
// 	if !ok {
// 		address.TagMap["Name"] = types.Tag{
// 			Value: aws.String("-"),
// 		}
// 	}
// 	return &addressWithTags{
// 		Address: address.Address,
// 		TagMap:  address.TagMap,
// 	}, nil
// }

// Legacy pretty-table printer removed in favor of streaming output for low memory usage.
