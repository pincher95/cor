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
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
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
			{
				Name: "delete",
				Type: "bool",
			},
		}

		flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
		if err != nil {
			logger.LogError("Error getting flags", err, nil, true)
			return
		}

		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String(flagValues["auth-method"].(string)),
			Profile:    aws.String(flagValues["profile"].(string)),
			Region:     aws.String(flagValues["region"].(string)),
		}

		cfg, err := handlers.NewConfigV2(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			logger.LogError("Failed loading AWS client config", err, nil, true)
			return
		}

		ec2Client := ec2.NewFromConfig(*cfg)
		stsClient := sts.NewFromConfig(*cfg)

		selfAccount, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			logger.LogError("Failed loading AWS client config", err, nil, true)
		}

		var wg sync.WaitGroup
		var tableRows []table.Row
		var handleSnapshots []snapshotWithTags
		var handleVolumesIDs []volumeWithTags

		snapshotChan := make(chan snapshotWithTags, 500)
		volumeIDsChan := make(chan volumeWithTags, 500)
		tableRowChan := make(chan *table.Row, 100)
		errorChan := make(chan error, 1)
		doneChan := make(chan struct{})

		// Start a goroutine to process volumes
		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Println("Starting volume processing goroutine")
			if err := describeVolumes(ctx, ec2Client, volumeIDsChan, []types.Filter{}); err != nil {
				errorChan <- err
				close(volumeIDsChan)
				return
			}
			// Close the channel after sending all snapshots
			for volumeWithTags := range volumeIDsChan {
				handleVolumesIDs = append(handleVolumesIDs, volumeWithTags)
			}
			fmt.Println("Volumes processed:", len(handleVolumesIDs))
			fmt.Println("Volume processing goroutine done")
		}()

		// Start a goroutine to process snapshots
		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Println("Starting snapshots processing goroutine")
			filter := []types.Filter{
				{
					Name:   aws.String("owner-id"),
					Values: []string{*selfAccount.Account},
				},
				{
					Name:   aws.String("tag:Name"),
					Values: []string{flagValues["filter-by-name"].(string)},
				},
			}
			if err := describeSnapshots(ctx, ec2Client, snapshotChan, filter); err != nil {
				errorChan <- err
				return
			}
			for snapshotWithTags := range snapshotChan {
				handleSnapshots = append(handleSnapshots, snapshotWithTags)
			}
			fmt.Println("Snapshots processed:", len(handleSnapshots))
			fmt.Println("Snapshots processing goroutine done")
		}()

		// Start a goroutine to wait for all processing to complete
		go func() {
			wg.Wait()
			fmt.Println("Wait group done")
			close(doneChan)
		}()

		for {
			select {
			case err := <-errorChan:
				logger.LogError("Error during loadbalancer processing", err, nil, true)
				return
			case <-doneChan:
				defer close(tableRowChan)
				fmt.Println("Processing done")
				fmt.Println("Snapshots processed:", len(handleSnapshots))
				fmt.Println("Volumes processed:", len(handleVolumesIDs))
				handlerSnapshot(handleVolumesIDs, handleSnapshots, tableRowChan)
				for row := range tableRowChan {
					tableRows = append(tableRows, *row)
					fmt.Println("Row added")
					fmt.Println("Row:", *row)
				}
				columnConfig := []table.ColumnConfig{
					{Name: "Name", AlignHeader: text.AlignCenter},
					{Name: "Snapshot ID", AlignHeader: text.AlignCenter},
					{Name: "Size", AlignHeader: text.AlignCenter},
				}

				printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"Name", "Snapshot ID", "Size"}, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}}, &columnConfig)

				if err := printerClient.PrintTextTable(&tableRows); err != nil {
					logger.LogError("Error printing table", err, nil, false)
				}
			}
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

func describeSnapshots(ctx context.Context, client *ec2.Client, snapshotChan chan<- snapshotWithTags, filter []types.Filter) error {
	// Create a paginator for Snapshots owned by self
	fmt.Println("Starting describeSnapshots")
	paginator := ec2.NewDescribeSnapshotsPaginator(client, &ec2.DescribeSnapshotsInput{
		MaxResults: aws.Int32(500),
		Filters:    filter,
	})

	// Iterate over the pages
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			close(snapshotChan)
			return err
		}

		// Send snapshots to the channel
		for _, snapshot := range output.Snapshots {
			tagMap := utils.TagsToMap(snapshot.Tags)
			snapshotChan <- snapshotWithTags{Snapshot: snapshot, TagMap: tagMap}
		}
	}
	close(snapshotChan)
	fmt.Println("Finished describeSnapshots")
	return nil
}

func handlerSnapshot(handleVolumesIDs []volumeWithTags, handleSnapshots []snapshotWithTags, tableRowChan chan<- *table.Row) {
	fmt.Println("Starting handlerSnapshot")
	volumeSnapshotID := make(map[string]bool)

	fmt.Println("Snapshots processed:", len(handleSnapshots))
	// fmt.Println("Snapshots:", handleSnapshots)

	fmt.Println("Volumes processed:", len(handleVolumesIDs))
	// fmt.Println("Volumes:", handleVolumesIDs)

	// Create a slice to store the total size of all volumes
	size := int32(0)
	// Process volume IDs
	for _, volumeWithTags := range handleVolumesIDs {
		snapshotID := volumeWithTags.Volume.SnapshotId
		if strings.Contains(*snapshotID, "snap") {
			volumeSnapshotID[*snapshotID] = true
		}
		volumeSnapshotID[*snapshotID] = false
	}

	for _, snapshotWithTags := range handleSnapshots {
		snapshotVolumeID := snapshotWithTags.Snapshot.VolumeId
		snapshotSize := snapshotWithTags.Snapshot.VolumeSize
		tagMap := snapshotWithTags.TagMap
		nameTag, ok := tagMap["Name"]
		if !ok {
			nameTag = types.Tag{Value: aws.String("-")}
		}

		// if !strings.Contains(*snapshotWithTags.Snapshot.Description, "Created by CreateImage") || !strings.Contains(*snapshotWithTags.Snapshot.Description, "Created for policy") {
		if volumeSnapshotID[*snapshotWithTags.Snapshot.SnapshotId] {
			tableRowChan <- &table.Row{
				*nameTag.Value,
				*snapshotVolumeID,
				*snapshotSize,
			}
			size += *snapshotSize
		}
		// }
	}
	tableRowChan <- &table.Row{"Total", "", size}
	// close(tableRowChan)
}
