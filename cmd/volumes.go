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
	"github.com/pincher95/cor/pkg/handlers"
	"github.com/pincher95/cor/pkg/printer"
	"github.com/pincher95/cor/pkg/utils"
	"github.com/spf13/cobra"
)

type volumeWithTags struct {
	Volume types.Volume
	TagMap map[string]types.Tag
}

// volumesListCmd represents the volumes command
var volumesCmd = &cobra.Command{
	Use:   "volumes",
	Short: "A brief description of your command",
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

		volumeName, err := cmd.Flags().GetString("filter-by-name")
		if err != nil {
			fmt.Println(err)
		}

		cfg, err := handlers.NewConfig(authMethod, profile, region, "UTC", true, true)
		if err != nil {
			panic(fmt.Sprintf("failed loading config, %v", err))
		}

		ec2Client := ec2.NewFromConfig(*cfg)

		// Create a sync.Pool to reuse table rows
		rowPool := &sync.Pool{
			New: func() interface{} {
				return make([]table.Row, 0, 500)
			},
		}

		tableRows := rowPool.Get().([]table.Row)

		var wg sync.WaitGroup

		// Create a channel to process volumes concurrently
		volumeChan := make(chan volumeWithTags, 500)

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(volumeChan)
			describeVolumes(context.TODO(), ec2Client, volumeChan, &volumeName)
		}()

		// Start a goroutine to process volumes
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Create a slice to store the total size of all volumes
			size := int32(0)
			for volumeWithTags := range volumeChan {
				volume := volumeWithTags.Volume
				tagMap := volumeWithTags.TagMap

				nameTag, ok := tagMap["Name"]
				if !ok {
					nameTag = types.Tag{Value: aws.String("-")}
				}

				tableRows = append(tableRows, table.Row{
					*nameTag.Value,
					*volume.VolumeId,
					*volume.SnapshotId,
					*volume.Size,
				})

				// Put the table rows back into the pool
				rowPool.Put(tableRows)

				size += *volume.Size
			}
			tableRows = append(tableRows, table.Row{"", "", "", size, "Total"})
		}()

		// Wait for all goroutines to finish
		wg.Wait()

		columnConfig := []table.ColumnConfig{
			{Name: "Name", AlignHeader: text.AlignCenter},
			{Name: "Volume ID", AlignHeader: text.AlignCenter},
			{Name: "Snapshot ID", AlignHeader: text.AlignCenter},
			{Name: "Size", AlignHeader: text.AlignCenter},
		}

		printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"Name", "Volume ID", "Snapshot ID", "Size"}, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}}, &columnConfig)

		if err := printerClient.PrintTextTable(&tableRows); err != nil {
			fmt.Printf("%v", err)
		}
	},
}

func init() {
	// rootCmd.AddCommand(volumesCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// volumesCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// volumesCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
	volumesCmd.Flags().String("filter-by-name", "*", "The name of the volume (provided during volume creation) ,You can use a wildcard ( * ), for example, 2021-09-29T* , which matches an entire day.")
}

func describeVolumes(ctx context.Context, client *ec2.Client, volumeChan chan<- volumeWithTags, volumeName *string) error {
	// Create a paginator
	paginator := ec2.NewDescribeVolumesPaginator(client, &ec2.DescribeVolumesInput{
		Filters: []types.Filter{
			{
				Name: aws.String("status"),
				Values: []string{
					"available",
				},
			},
			{
				Name:   aws.String("tag:Name"),
				Values: []string{*volumeName},
			},
		},
	})

	// Iterate over the pages
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}

		// Send volumes to the channel
		for _, volume := range output.Volumes {
			tagMap := utils.TagsToMap(volume.Tags)
			volumeChan <- volumeWithTags{Volume: volume, TagMap: tagMap}
		}
	}

	return nil
}
