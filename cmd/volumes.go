/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
	"io"
	"os"

	"golang.org/x/sync/errgroup"

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
)

type volumeWithTags struct {
	Volume types.Volume
	TagMap map[string]types.Tag
}

type volumeResult struct {
	row  table.Row
	size int32
}

// volumesListCmd represents the volumes command
var volumesCmd = &cobra.Command{
	Use:   "volumes",
	Short: "A brief description of your command",
	Long:  ``,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Create prompter using the prompter package
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout

		// Create a context
		ctx := cmd.Context()

		// Get the flags from the command and also the additional flags specific to this command
		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		// Specify additional flags that are specific to this command
		additionalFlags := []flags.Flag{
			{
				Name: "filter-by-name",
				Type: "string",
			},
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

		// Create a new EC2 client
		ec2Client := ec2.NewFromConfig(*cfg)

		awsClient := &handlers.AWSClientImpl{
			EC2: ec2Client,
		}

		return runVolumeCmd(ctx, &prompterClient, output, awsClient, flagValues)
	},
}

func runVolumeCmd(ctx context.Context, prompter *prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	// Create an instance of AWSCommand
	volumeCmd := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  *prompter,
		Output:    output,
	}

	return volumeCmd.executeVolumes(ctx, flagValues)
}

func (v *AWSCommand) executeVolumes(ctx context.Context, flagValues *map[string]any) error {
	// Create a channel to process volumes
	volumeWithTagsChan := make(chan volumeWithTags, 10)
	resultsChan := make(chan volumeResult, 10)

	// Create an errgroup with context
	g, ctx := errgroup.WithContext(ctx)

	// Goroutine to describe volumes
	g.Go(func() error {
		volumeFilter := []types.Filter{
			{
				Name:   aws.String("status"),
				Values: []string{"available"},
			},
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
		if err := v.DescribeVolumes(ctx, volumeWithTagsChan, &volumeFilter); err != nil {
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
					return ctx.Err()
				case volume, ok := <-volumeWithTagsChan:
					if !ok {
						return nil
					}
					processedVolume, err := handleVolume(volume)
					if err != nil {
						return err
					}
					// Safely dereference pointers with nil checks
					name := "-"
					if tag, exists := processedVolume.TagMap["Name"]; exists && tag.Value != nil {
						name = *tag.Value
					}
					volID := ""
					if processedVolume.Volume.VolumeId != nil {
						volID = *processedVolume.Volume.VolumeId
					}
					snapshotID := ""
					if processedVolume.Volume.SnapshotId != nil {
						snapshotID = *processedVolume.Volume.SnapshotId
					}
					size := int32(0)
					if processedVolume.Volume.Size != nil {
						size = *processedVolume.Volume.Size
					}
					row := table.Row{name, volID, snapshotID, size}
					resultsChan <- volumeResult{row: row, size: size}
				}
			}
		})
	}

	// Result collector goroutine: concurrently reads from resultsChan.
	resultCollectorDone := make(chan struct{})
	tableRows := make([]table.Row, 0)
	var totalSize int32
	go func() {
		for res := range resultsChan {
			tableRows = append(tableRows, res.row)
			totalSize += res.size
		}
		close(resultCollectorDone)
	}()

	// Wait for the describer and workers to finish.
	if err := g.Wait(); err != nil {
		v.Logger.LogError("Error during volume processing", err, nil, false)
		return err
	}

	// All worker and describer goroutines are done; close the results channel.
	close(resultsChan)
	// Wait for the collector to finish.
	<-resultCollectorDone

	// Append total row
	tableRows = append(tableRows, table.Row{"", "", "", totalSize, "Total"})

	// Print the table
	if err := printVolumeTable(&tableRows); err != nil {
		v.Logger.LogError("Error printing volume table", err, nil, false)
		return err
	}

	if (*flagValues)["delete"].(bool) {
		if err := v.deleteVolumes(ctx, &tableRows); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	volumesCmd.Flags().String("filter-by-name", "*", "The name of the volume (provided during volume creation) ,You can use a wildcard ( * ), for example, 2021-09-29T* , which matches an entire day.")
}

// deleteVolumes deletes the volumes based on the user confirmation
func (v *AWSCommand) deleteVolumes(ctx context.Context, tableRows *[]table.Row) error {
	confirm, err := v.Prompter.Confirm("Are you sure you want to proceed? (yes/no): ")
	if err != nil {
		v.Logger.LogError("Error during user prompt", err, nil, false)
		return err
	}

	if confirm == nil {
		v.Logger.LogInfo("Invalid response. Please enter 'yes' or 'no'.", nil)
		return err
	} else if *confirm {
		for _, tableRow := range *tableRows {
			// Skip the last row which is the total
			if total, ok := tableRow[4].(string); ok && total == "Total" {
				continue
			}
			// Delete the volume
			v.Logger.LogInfo("Deleting Volumes", map[string]any{"VolumeName": tableRow[0].(string)})
			_, err := v.AWSClient.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
				VolumeId: aws.String(tableRow[1].(string)),
			})
			if err != nil {
				v.Logger.LogError("Error deleting volume", err, nil, false)
				return err
			}
		}
	} else if !*confirm {
		v.Logger.LogInfo("Aborted.", nil)
	}

	return nil
}

// DescribeVolumes describes the volumes based on the filter provided
func (v *AWSCommand) DescribeVolumes(ctx context.Context, volumeWithTagsChan chan<- volumeWithTags, filters *[]types.Filter) error {
	defer func() {
		if recover() != nil {
			// Prevent panic if the channel is already closed
			v.Logger.LogError("Channel `volumeWithTagsChan` closed", nil, nil, false)

		}
		close(volumeWithTagsChan)
	}()

	// If filters are nil, create an empty filter
	if filters == nil {
		filters = &[]types.Filter{}
	}

	// Create a paginator
	paginator := ec2.NewDescribeVolumesPaginator(v.AWSClient.EC2, &ec2.DescribeVolumesInput{
		Filters: *filters,
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
			volumeWithTagsChan <- volumeWithTags{Volume: volume, TagMap: tagMap}
		}
	}

	return nil
}

// handleVolume handles the volume and adds a default name if not present
func handleVolume(volume volumeWithTags) (*volumeWithTags, error) {
	_, ok := volume.TagMap["Name"]
	if !ok {
		volume.TagMap["Name"] = types.Tag{
			Value: aws.String("-"),
		}
	}
	return &volumeWithTags{
		Volume: volume.Volume,
		TagMap: volume.TagMap,
	}, nil
}

// getColumnConfig returns the column configuration for the volume table
func getVolumeColumnConfig() *[]table.ColumnConfig {
	return &[]table.ColumnConfig{
		{
			Name:        "Name",
			AlignHeader: text.AlignCenter,
		},
		{
			Name:        "Volume ID",
			AlignHeader: text.AlignCenter,
		},
		{
			Name:        "Snapshot ID",
			AlignHeader: text.AlignCenter,
		},
		{
			Name:        "Size",
			AlignHeader: text.AlignCenter,
		},
	}
}

// printVolumeTable prints the volume table
func printVolumeTable(tableRows *[]table.Row) error {

	columnConfig := getVolumeColumnConfig()
	sortConfig := []table.SortBy{{Name: "Name", Mode: table.Dsc}}

	return printTable(columnConfig, &table.Row{"Name", "Volume ID", "Snapshot ID", "Size"}, tableRows, &sortConfig)
}
