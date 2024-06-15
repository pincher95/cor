/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
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

type snapshotWithTags struct {
	Snapshot types.Snapshot
	TagMap   map[string]types.Tag
}

// snapshotsCmd represents the snapshots command
var snapshotsCmd = &cobra.Command{
	Use:   "snapshots",
	Short: "Return Snapshots not associated with AMI, Volumes or created by Lifecycle policy",
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

		snapshotName, err := cmd.Flags().GetString("filter-by-name")
		if err != nil {
			fmt.Println(err)
		}

		cfg, err := handlers.NewConfig(authMethod, profile, region, "UTC", true, true)
		if err != nil {
			panic(fmt.Sprintf("failed loading config, %v", err))
		}

		ec2Client := ec2.NewFromConfig(*cfg)

		// Create channels to process/volumes/snapshot volume IDs concurrently
		snapshotChan := make(chan snapshotWithTags, 500)
		volumeIDs := make(chan volumeWithTags, 500)
		snapshotVolumeIDs := make(chan string, 500)

		// Create a slice to store the total size of all volumes
		size := int32(0)

		// Create a sync.Pool to reuse table rows
		rowPool := &sync.Pool{
			New: func() interface{} {
				return make([]table.Row, 0, 500)
			},
		}

		tableRows := rowPool.Get().([]table.Row)

		var wg sync.WaitGroup

		go func(snapshotName *string) {
			wg.Add(1)
			defer wg.Done()
			defer close(snapshotChan)
			describeSnapshots(context.TODO(), ec2Client, snapshotChan, snapshotName)
		}(&snapshotName)

		go func() {
			wg.Add(1)
			defer wg.Done()

			for snapshotWithTags := range snapshotChan {
				volumeID := snapshotWithTags.Snapshot.VolumeId
				volumeSize := snapshotWithTags.Snapshot.VolumeSize
				tagMap := snapshotWithTags.TagMap
				nameTag, ok := tagMap["Name"]
				if !ok {
					nameTag = types.Tag{Value: aws.String("-")}
				}

				if !strings.Contains(*snapshotWithTags.Snapshot.Description, "Created by CreateImage") && !strings.Contains(*snapshotWithTags.Snapshot.Description, "Created for policy") {
					snapshotVolumeIDs <- *volumeID
				}

				tableRows = append(tableRows, table.Row{
					*nameTag.Value,
					*volumeID,
					*volumeSize,
				})

				size += *volumeSize
			}

			// Put the table rows back into the pool
			rowPool.Put(tableRows)

			tableRows = append(tableRows, table.Row{"", "", size, "Total"})

			close(snapshotVolumeIDs)
		}()

		// Start a goroutine to process volumes
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(volumeIDs)
			describeVolumes(context.TODO(), ec2Client, volumeIDs, aws.String("*"))
		}()

		// Close the channel after all volumes have been processed
		// close(snapshotChan)

		// tempVolumeIDs := make([]string, 0)
		// for volumeWithTags := range volumeIDs {
		// 	snapshotID := volumeWithTags.Volume.SnapshotId
		// 	volumeID := volumeWithTags.Volume.VolumeId
		// 	if strings.Contains(*snapshotID, "snap") {
		// 		tempVolumeIDs = append(tempVolumeIDs, *volumeID)
		// 		fmt.Println(tempVolumeIDs)
		// 	}
		// }

		// Process volume IDs
		tempVolumeIDs := make([]string, 0)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for volumeWithTags := range volumeIDs {
				snapshotID := volumeWithTags.Volume.SnapshotId
				volumeID := volumeWithTags.Volume.VolumeId
				if strings.Contains(*snapshotID, "snap") {
					tempVolumeIDs = append(tempVolumeIDs, *volumeID)
				}
			}
		}()

		// Wait for all goroutines to finish
		wg.Wait()

		// sorted := utils.RemoveFromSlice(snapshotVolumeIDs, tempVolumeIDs)

		// for _, chunk := range utils.SliceChunkBy(sorted, 199) {
		// 	// snapshots_final, err := ec2Client.DescribeSnapshots(context.TODO(), &ec2.DescribeSnapshotsInput{
		// 	_, err := ec2Client.DescribeSnapshots(context.TODO(), &ec2.DescribeSnapshotsInput{
		// 		Filters: []types.Filter{
		// 			{
		// 				Name: aws.String("owner-id"),
		// 				Values: []string{
		// 					"406095609952",
		// 				},
		// 			},
		// 			{
		// 				Name:   aws.String("volume-id"),
		// 				Values: chunk,
		// 			},
		// 		},
		// 		// MaxResults: aws.Int32(1000),
		// 	})
		// 	if err != nil {
		// 		fmt.Println(err)
		// 	}
		// }

		columnConfig := []table.ColumnConfig{
			{Name: "Name", AlignHeader: text.AlignCenter},
			{Name: "Snapshot ID", AlignHeader: text.AlignCenter},
			{Name: "Size", AlignHeader: text.AlignCenter},
		}

		printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"Name", "Snapshot ID", "Size"}, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}}, &columnConfig)

		if err := printerClient.PrintTextTable(&tableRows); err != nil {
			fmt.Printf("%v", err)
		}
	},
}

func init() {
	// rootCmd.AddCommand(snapshotsCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// snapshotsCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// snapshotsCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
	snapshotsCmd.Flags().String("filter-by-name", "*", "The name of the volume (provided during volume creation) ,You can use a wildcard ( * ).")
}

func describeSnapshots(ctx context.Context, client *ec2.Client, snapshotChan chan<- snapshotWithTags, snapshotName *string) {
	// Create a paginator for Snapshots owned by self
	paginator := ec2.NewDescribeSnapshotsPaginator(client, &ec2.DescribeSnapshotsInput{
		MaxResults: aws.Int32(500),
		Filters: []types.Filter{
			{
				Name: aws.String("owner-id"),
				Values: []string{
					"406095609952",
				},
			},
			{
				Name:   aws.String("tag:Name"),
				Values: []string{*snapshotName},
			},
		},
	})

	// Iterate over the pages
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			fmt.Println(err)
			continue
		}

		// Send snapshots to the channel
		for _, snapshot := range output.Snapshots {
			tagMap := utils.TagsToMap(snapshot.Tags)
			snapshotChan <- snapshotWithTags{Snapshot: snapshot, TagMap: tagMap}
		}
	}
}
