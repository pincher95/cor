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

	"golang.org/x/sync/errgroup"

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
	Short: "List and optionally delete unattached EBS volumes",
	Long:  `List EBS volumes in 'available' state (not attached to any instance) and optionally delete them.`,
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
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  *prompter,
		Output:    output,
	}

	return command.executeVolumes(ctx, flagValues)
}

func (v *AWSCommand) executeVolumes(ctx context.Context, flagValues *map[string]any) error {
	// Preserve the original context for delete operations (avoid errgroup ctx cancellation).
	rootCtx := ctx

	// If deleting, confirm up-front so we can stream without buffering IDs.
	doDelete := false
	if (*flagValues)["delete"].(bool) {
		confirm, err := v.Prompter.Confirm("Are you sure you want to proceed? (yes/no): ")
		if err != nil {
			v.Logger.LogError("Error during user prompt", err, nil, false)
			return err
		}
		if confirm == nil || !*confirm {
			v.Logger.LogInfo("Aborted.", nil)
			return nil
		}
		doDelete = true
	}

	// Create a channel to process volumes
	volumeWithTagsChan := make(chan volumeWithTags, 10)
	resultsChan := make(chan volumeResult, 10)

	// Create an errgroup with context
	g, egCtx := errgroup.WithContext(ctx)

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
		if err := v.DescribeVolumes(egCtx, volumeWithTagsChan, &volumeFilter); err != nil {
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
					return egCtx.Err()
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
	var totalSize int32
	go func() {
		stream := printer.NewStreamTable(v.Output, true, []string{"Name", "Volume ID", "Snapshot ID", "Size"})
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))

		for res := range resultsChan {
			stream.WriteRow(res.row...)
			totalSize += res.size

			if doDelete {
				// row = Name, VolumeId, SnapshotId, Size
				if len(res.row) < 2 {
					continue
				}
				volID, _ := res.row[1].(string)
				if volID == "" {
					continue
				}
				v.Logger.LogInfo("Deleting Volumes", map[string]any{"VolumeId": volID})
				if _, err := v.AWSClient.DeleteVolume(rootCtx, &ec2.DeleteVolumeInput{VolumeId: aws.String(volID)}); err != nil {
					v.Logger.LogError("Error deleting volume", err, map[string]any{"VolumeId": volID}, false)
					// Cancel the group by returning; main goroutine will see ctx.Done via errgroup.
					break
				}
			}
		}
		stream.WriteRow("Total", "", "", totalSize)
		stream.Close()
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

	return nil
}

func init() {
	volumesCmd.Flags().String("filter-by-name", "*", "Filter volumes by tag:Name (wildcards supported, e.g. 'foo*').")
}

// DescribeVolumes describes the volumes based on the filter provided
func (v *AWSCommand) DescribeVolumes(ctx context.Context, volumeWithTagsChan chan<- volumeWithTags, filters *[]types.Filter) error {
	defer close(volumeWithTagsChan)

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
