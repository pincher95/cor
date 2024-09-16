/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"bufio"
	"context"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
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

		var wg sync.WaitGroup

		// Create a channel to process volumes concurrently
		volumeChan := make(chan volumeWithTags, 10)
		volumeWithTagsChan := make(chan volumeWithTags, 10)
		errorChan := make(chan error, 1)
		doneChan := make(chan struct{})

		// Create a slice of table.Row
		var tableRows []table.Row

		wg.Add(1)
		go func() {
			defer wg.Done()
			volumeFilter := []types.Filter{
				{
					Name:   aws.String("status"),
					Values: []string{"available"},
				},
				{
					Name:   aws.String("tag:Name"),
					Values: []string{flagValues["filter-by-name"].(string)},
				},
			}
			if err := describeVolumes(ctx, ec2Client, volumeChan, volumeFilter); err != nil {
				errorChan <- err
				return
			}
		}()

		// Wait for all goroutines to finish
		go func() {
			wg.Wait()
			close(doneChan)
		}()

		for {
			select {
			case err := <-errorChan:
				logger.LogError("Error during volume processing", err, nil, true)
				return
			case <-doneChan:
				close(volumeWithTagsChan)
				size := int32(0)
				for row := range volumeChan {
					volumeWithTags, err := handleVolume(row)
					if err != nil {
						errorChan <- err
						return
					}

					tableRows = append(tableRows, table.Row{
						*volumeWithTags.TagMap["Name"].Value,
						*volumeWithTags.Volume.VolumeId,
						*volumeWithTags.Volume.SnapshotId,
						*volumeWithTags.Volume.Size,
					})
					size += *volumeWithTags.Volume.Size
				}
				tableRows = append(tableRows, table.Row{"", "", "", size, "Total"})
				columnConfig := []table.ColumnConfig{
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

				printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"Name", "Volume ID", "Snapshot ID", "Size"}, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}}, &columnConfig)

				if err := printerClient.PrintTextTable(&tableRows); err != nil {
					logger.LogError("Error printing table", err, nil, false)
				}

				if flagValues["delete"].(bool) {
					reader := bufio.NewReader(os.Stdin)
					logger.LogInfo("Are you sure you want to proceed? (yes/no): ", nil)
					response, _ := reader.ReadString('\n')
					response = strings.ToLower(strings.TrimSpace(response))

					if response == "yes" || response == "y" {
						for _, tableRow := range tableRows {
							// Skip the last row which is the total
							if tableRow[4].(string) == "Total" {
								continue
							}
							// Delete the volume
							logger.LogInfo("Deleting Volumes", map[string]interface{}{"VolumeName": tableRow[0].(string)})
							_, err := ec2Client.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
								VolumeId: aws.String(tableRow[1].(string)),
							})
							if err != nil {
								logger.LogError("Error deleting volume", err, nil, false)
							}
						}
					} else if response == "no" || response == "n" {
						logger.LogInfo("Aborted.", nil)
					} else {
						logger.LogInfo("Invalid response. Please enter 'yes' or 'no'.", nil)
					}
				}
				return
			}
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

func describeVolumes(ctx context.Context, client *ec2.Client, volumeChan chan<- volumeWithTags, filter []types.Filter) error {
	// Create a paginator
	paginator := ec2.NewDescribeVolumesPaginator(client, &ec2.DescribeVolumesInput{
		Filters: filter,
	})

	// Iterate over the pages
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			close(volumeChan)
			return err
		}

		// Send volumes to the channel
		for _, volume := range output.Volumes {
			tagMap := utils.TagsToMap(volume.Tags)
			volumeChan <- volumeWithTags{Volume: volume, TagMap: tagMap}
		}
	}
	close(volumeChan)
	return nil
}

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
