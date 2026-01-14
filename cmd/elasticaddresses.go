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
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
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
		// Create prompter using the prompter package
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout

		// Create a context
		ctx := cmd.Context()

		// Create a new logger and error handler
		logger := logging.NewLogger()

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
			logger.LogError("Error getting flags", err, nil, true)
			return err
		}

		// Create AWS client configuration
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

		// Create a new EC2 client
		ec2Client := ec2.NewFromConfig(*cfg)

		awsClient := &handlers.AWSClientImpl{
			EC2: ec2Client,
		}

		return runElasticIPsCmd(ctx, &prompterClient, output, awsClient, flagValues)
	},
}

func init() {
	elasticIPsCmd.Flags().String("filter-by-name", "*", "Filter Elastic IPs by tag:Name (wildcards supported, e.g. 'foo*').")
}

func runElasticIPsCmd(ctx context.Context, prompter *prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	// Create an instance of elbv2Command
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  *prompter,
		Output:    output,
	}

	return command.executeElasticIPs(ctx, flagValues)
}

func (a *AWSCommand) executeElasticIPs(ctx context.Context, flagValues *map[string]any) error {
	// Preserve the original context for delete operations (avoid errgroup ctx cancellation).
	rootCtx := ctx

	// If deleting, confirm up-front so we can stream without buffering IDs.
	doDelete := false
	if (*flagValues)["delete"].(bool) {
		confirm, err := a.Prompter.Confirm("Are you sure you want to proceed? (yes/no): ")
		if err != nil {
			a.Logger.LogError("Error during user prompt", err, nil, false)
			return err
		}
		if confirm == nil || !*confirm {
			a.Logger.LogInfo("Aborted.", nil)
			return nil
		}
		doDelete = true
	}

	// Create a channel to process addresses
	addressChan := make(chan addressWithTags, 10)
	resultsChan := make(chan table.Row, 10)

	// Create an errgroup with context
	g, egCtx := errgroup.WithContext(ctx)

	// Goroutine to describe volumes
	g.Go(func() error {
		elasticIPFilter := []types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: func() []string {
					if filterByName, ok := (*flagValues)["filter-by-name"].(string); ok {
						return []string{filterByName}
					}
					return []string{}
				}(),
			},
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
	resultDone := make(chan struct{})
	go func() {
		stream := printer.NewStreamTable(a.Output, true, []string{"Name", "Allocation ID", "Allocated Public address", "Association ID", "Network interface ID"})
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))

		for row := range resultsChan {
			stream.WriteRow(row...)

			if doDelete {
				// Expect: Name, AllocationID, PublicIP, AssociationID, NetworkInterfaceID
				if len(row) < 3 {
					continue
				}

				allocationID, _ := row[1].(string)
				publicIP, _ := row[2].(string)

				// Prefer AllocationId (VPC EIPs), fall back to PublicIp (EC2-Classic).
				input := &ec2.ReleaseAddressInput{}
				if allocationID != "" && allocationID != "-" {
					input.AllocationId = aws.String(allocationID)
				} else if publicIP != "" && publicIP != "-" {
					input.PublicIp = aws.String(publicIP)
				} else {
					continue
				}

				a.Logger.LogInfo("Releasing Elastic IP", map[string]any{"AllocationId": allocationID, "PublicIp": publicIP})
				if _, err := a.AWSClient.EC2.ReleaseAddress(rootCtx, input); err != nil {
					a.Logger.LogError("Error releasing Elastic IP", err, map[string]any{"AllocationId": allocationID, "PublicIp": publicIP}, false)
					// fail-fast
					break
				}
			}
		}
		stream.Close()
		close(resultDone)
	}()

	// Wait for the describer and workers to finish.
	if err := g.Wait(); err != nil {
		a.Logger.LogError("Error during volume processing", err, nil, false)
		return err
	}

	// All worker and describer goroutines are done; close the results channel.
	close(resultsChan)
	<-resultDone

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
