/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
	"io"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/promter"
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
	RunE: func(cmd *cobra.Command, args []string) error {
		// Create prompter using the prompter package
		prompterClient := promter.NewConsolePrompter(os.Stdin, os.Stdout)
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

		cfg, err := handlers.NewConfigV2(ctx, *cloudConfig, "UTC", true, true)
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

func runVolumeCmd(ctx context.Context, prompter *promter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]interface{}) error {
	// Create an instance of elbv2Command
	elbCmd := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  *prompter,
		Output:    output,
	}

	return elbCmd.executeELB(ctx, flagValues)
}

func (v *AWSCommand) executeELB(ctx context.Context, flagValues *map[string]interface{}) error {
	// Create a channel to process volumes concurrently
	volumeWithTagsChan := make(chan volumeWithTags, 10)
	errorChan := make(chan error, 1)
	doneChan := make(chan struct{})

	// Create a wait group
	var wg sync.WaitGroup

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
				Values: []string{(*flagValues)["filter-by-name"].(string)},
			},
		}
		if err := v.DescribeVolumes(ctx, volumeWithTagsChan, &volumeFilter); err != nil {
			errorChan <- err
			return
		}
	}()

	// Wait for all goroutines to finish
	go func() {
		wg.Wait()
		close(doneChan)
	}()

	// Dynamically collect results and process them in a select loop
	var tableRows []table.Row
	var totalSize int32

	// Process the volumes concurrently
	for {
		select {
		case err := <-errorChan:
			v.Logger.LogError("Error during volume processing", err, nil, false)
			return err
		case <-doneChan:
			// Process volume and accumulate results
			for volume := range volumeWithTagsChan {
				volumeWithTags, err := handleVolume(volume)
				if err != nil {
					v.Logger.LogError("Error handling volume", err, nil, false)
					return err
				}

				// Append each processed volume row
				tableRows = append(tableRows, table.Row{
					*volumeWithTags.TagMap["Name"].Value,
					*volumeWithTags.Volume.VolumeId,
					*volumeWithTags.Volume.SnapshotId,
					*volumeWithTags.Volume.Size,
				})

				// Keep track of the total size
				totalSize += *volumeWithTags.Volume.Size
			}

			// When done, add the total row and break out of the loop
			tableRows = append(tableRows, table.Row{"", "", "", totalSize, "Total"})

			// Print the volume table
			if err := printVolumeTable(&tableRows); err != nil {
				v.Logger.LogError("Error printing volume table", err, nil, false)
				errorChan <- err
			}

			// Handle volume deletion based on flag
			if (*flagValues)["delete"].(bool) {
				if err := v.deleteVolumes(ctx, &tableRows); err != nil {
					return err
				}
			}
			return nil
		}
	}
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
			if tableRow[4].(string) == "Total" {
				continue
			}
			// Delete the volume
			v.Logger.LogInfo("Deleting Volumes", map[string]interface{}{"VolumeName": tableRow[0].(string)})
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

// printVolumeTable prints the volume table
func printVolumeTable(tableRows *[]table.Row) error {

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

	return printTable(&columnConfig, &table.Row{"Name", "Volume ID", "Snapshot ID", "Size"}, tableRows, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}})
}
