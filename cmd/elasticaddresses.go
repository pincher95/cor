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
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/utils"
	"github.com/spf13/cobra"
)

type addressWithTags struct {
	Address types.Address
	TagMap  map[string]types.Tag
}

// elasticaddressesCmd represents the elasticaddresses command
var elasticaddressesCmd = &cobra.Command{
	Use:   "elasticaddresses",
	Short: "A brief description of your command",
	Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
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

		addressName, err := cmd.Flags().GetString("filter-by-name")
		if err != nil {
			fmt.Println(err)
		}

		cfg, err := handlers.NewConfig(authMethod, profile, region, "UTC", true, true)
		if err != nil {
			panic(fmt.Sprintf("failed loading config, %v", err))
		}

		ec2Client := ec2.NewFromConfig(*cfg)

		// Create a channel to process volumes concurrently
		addressChan := make(chan addressWithTags, 500)

		// Create a sync.Pool to reuse table rows
		rowPool := &sync.Pool{
			New: func() interface{} {
				return make([]table.Row, 0, 500)
			},
		}

		tableRows := rowPool.Get().([]table.Row)

		var wg sync.WaitGroup

		// Increment the WaitGroup counter
		wg.Add(1)

		// Create a goroutine to process elastic addresses concurrently
		go describeAddresses(context.TODO(), ec2Client, addressChan, &addressName, &wg)

		// Increment the WaitGroup counter
		wg.Add(1)
		go func() {
			defer wg.Done()

			for addressWithTags := range addressChan {
				address := addressWithTags.Address
				// tagMap := addressWithTags.TagMap

				if address.AssociationId == nil {
					if address.InstanceId == nil {

						tableRows = append(tableRows, table.Row{
							func(tags []types.Tag) string {
								nameTag, ok := utils.TagsToMap(address.Tags)["Name"]
								if !ok {
									return "-"
								}
								return *nameTag.Value
							}(address.Tags),
							*address.AllocationId,
							*address.PublicIp,
							func(AssociationId *string) string {
								if AssociationId == nil {
									return "-"
								}
								return *AssociationId
							}(address.AssociationId),
							func(networkInterfaceId *string) string {
								if networkInterfaceId == nil {
									return "-"
								}
								return *networkInterfaceId
							}(address.NetworkInterfaceId)})

					}
				}
			}
		}()

		// Wait for all goroutines to finish
		wg.Wait()

		columnConfig := []table.ColumnConfig{
			{Name: "Name", AlignHeader: text.AlignCenter},
			{Name: "Allocation ID", AlignHeader: text.AlignCenter},
			{Name: "Allocated Public address", AlignHeader: text.AlignCenter},
			{Name: "Association ID", AlignHeader: text.AlignCenter},
			{Name: "Network interface ID", AlignHeader: text.AlignCenter},
		}

		printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"Name", "Allocation ID", "Allocated Public address", "Association ID", "Network interface ID"}, &[]table.SortBy{{Name: "Name", Mode: table.Asc}}, &columnConfig)

		if err := printerClient.PrintTextTable(&tableRows); err != nil {
			fmt.Printf("%v", err)
		}
	},
}

func init() {
	// rootCmd.AddCommand(elasticaddressesCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// elasticaddressesCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// elasticaddressesCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
	elasticaddressesCmd.Flags().String("filter-by-name", "*", "The name of the volume (provided during volume creation) ,You can use a wildcard ( * ), for example, 2021-09-29T* , which matches an entire day.")
}

func describeAddresses(ctx context.Context, client *ec2.Client, addressChan chan<- addressWithTags, addressName *string, wg *sync.WaitGroup) {
	// Decrement the counter when the goroutine completes
	defer wg.Done()

	// Describe the addresses
	output, err := client.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:Name"),
				Values: []string{*addressName},
			},
		},
	})
	if err != nil {
		fmt.Println(err)
	}

	// Send volumes to the channel
	for _, address := range output.Addresses {
		tagMap := utils.TagsToMap(address.Tags)
		addressChan <- addressWithTags{Address: address, TagMap: tagMap}
	}

	// Close the channel
	close(addressChan)
}
