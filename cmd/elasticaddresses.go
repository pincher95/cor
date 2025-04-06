/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
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
	"github.com/jedib0t/go-pretty/v6/text"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
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
	Use:   "elasticaIPs",
	Short: "A brief description of your command",
	Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Create prompter using the prompter package
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout

		// Create a context
		ctx := context.TODO()

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
	elasticIPsCmd.Flags().String("filter-by-name", "*", "The name of the volume (provided during volume creation) ,You can use a wildcard ( * ), for example, 2021-09-29T* , which matches an entire day.")
}

func runElasticIPsCmd(ctx context.Context, prompter *prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	// Create an instance of elbv2Command
	elasticIPCmd := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  *prompter,
		Output:    output,
	}

	return elasticIPCmd.executeElasticIPs(ctx, flagValues)
}

func (a *AWSCommand) executeElasticIPs(ctx context.Context, flagValues *map[string]any) error {
	// Create a channel to process addresses
	addressChan := make(chan addressWithTags, 10)
	resultsChan := make(chan table.Row, 10)

	// Create an errgroup with context
	g, ctx := errgroup.WithContext(ctx)

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
		if err := a.describeAddresses(ctx, addressChan, &elasticIPFilter); err != nil {
			return err
		}
		return nil
	})

	// Launch worker goroutines
	numWorkers := 10
	for range numWorkers {
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
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
							if nameTag, ok := utils.TagsToMap(address.Tags)["Name"]; ok {
								name = *nameTag.Value
							}

							associationId := "-"
							if address.AssociationId != nil {
								associationId = *address.AssociationId
							}

							elasticIP := *address.PublicIp

							allocationId := *address.AllocationId

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

	// Result collector goroutine: concurrently reads from resultsChan.
	resultCollectorDone := make(chan struct{})
	var tableRows []table.Row
	go func() {
		for res := range resultsChan {
			tableRows = append(tableRows, res)
		}
		close(resultCollectorDone)
	}()

	// Wait for the describer and workers to finish.
	if err := g.Wait(); err != nil {
		a.Logger.LogError("Error during volume processing", err, nil, false)
		return err
	}

	// All worker and describer goroutines are done; close the results channel.
	close(resultsChan)
	// Wait for the collector to finish.
	<-resultCollectorDone

	// Print the table
	if err := printElasticIPsTable(&tableRows); err != nil {
		a.Logger.LogError("Error printing elastic IPs's table", err, nil, false)
		return err
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

func printElasticIPsTable(tableRows *[]table.Row) error {

	columnConfig := getElasticIPsColumnConfig()
	sortConfig := []table.SortBy{{Name: "Name", Mode: table.Asc}}

	return printTable(columnConfig, &table.Row{"Name", "Allocation ID", "Allocated Public address", "Association ID", "Network interface ID"}, tableRows, &sortConfig)
}

func getElasticIPsColumnConfig() *[]table.ColumnConfig {
	return &[]table.ColumnConfig{
		{
			Name:        "Name",
			AlignHeader: text.AlignCenter,
		},
		{
			Name:        "Allocation ID",
			AlignHeader: text.AlignCenter,
		},
		{
			Name:        "Allocated Public address",
			AlignHeader: text.AlignCenter,
		},
		{
			Name:        "Association ID",
			AlignHeader: text.AlignCenter,
		},
		{
			Name:        "Network interface ID",
			AlignHeader: text.AlignCenter,
		},
	}
}
