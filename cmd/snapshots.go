/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
	"io"
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
	"github.com/pincher95/cor/pkg/handlers/promter"
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
	RunE: func(cmd *cobra.Command, args []string) error {
		// Create prompter using the prompter package
		prompterClient := promter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout

		// Create a new context
		ctx := cmd.Context()

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
			return ctx.Err()
		}

		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String(flagValues["auth-method"].(string)),
			Profile:    aws.String(flagValues["profile"].(string)),
			Region:     aws.String(flagValues["region"].(string)),
		}

		cfg, err := handlers.NewConfigV2(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			return ctx.Err()
		}

		// Create a new EC2 client
		ec2Client := ec2.NewFromConfig(*cfg)
		// Create a new STS client
		stsClient := sts.NewFromConfig(*cfg)

		awsClient := &handlers.AWSClientImpl{
			EC2: ec2Client,
			STS: stsClient,
		}

		return runSnapshotCmd(ctx, prompterClient, output, *awsClient, flagValues)
	},
}

func runSnapshotCmd(ctx context.Context, prompter promter.Client, output io.Writer, awsClient handlers.AWSClientImpl, flagValues map[string]interface{}) error {

	// Create a new logger and error handler
	logger := logging.NewLogger()

	// Create an instance of elbv2Command
	snapshotCmd := &AWSCommand{
		AWSClient: awsClient,
		Logger:    logger,
		Prompter:  prompter,
		Output:    output,
	}

	return snapshotCmd.executeSnapShot(ctx, flagValues)
}

func (s *AWSCommand) executeSnapShot(ctx context.Context, flagValues map[string]interface{}) error {
	// Create channels to send snapshots and volumes
	snapshotChan := make(chan snapshotWithTags, 500)
	volumeIDsChan := make(chan volumeWithTags, 500)
	tableRowChan := make(chan *table.Row, 100)
	errorChan := make(chan error, 1)
	doneChan := make(chan struct{})

	selfAccount, err := s.AWSClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		s.Logger.LogError("Failed loading AWS client config", err, nil, false)
		errorChan <- err
	}

	var wg sync.WaitGroup
	var tableRows []table.Row
	var handleSnapshots []snapshotWithTags
	var handleVolumesIDs []volumeWithTags

	// Start a goroutine to process volumes
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.DescribeVolumes(ctx, volumeIDsChan, []types.Filter{}); err != nil {
			errorChan <- err
			return
		}
		// Close the channel after sending all snapshots
		for volumeWithTags := range volumeIDsChan {
			// fmt.Println(*volumeWithTags.Volume.VolumeId)
			handleVolumesIDs = append(handleVolumesIDs, volumeWithTags)
		}
	}()

	// Start a goroutine to process snapshots
	wg.Add(1)
	go func() {
		defer wg.Done()
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
		if err := s.describeSnapshots(ctx, snapshotChan, filter); err != nil {
			errorChan <- err
			return
		}
		for snapshotWithTags := range snapshotChan {
			handleSnapshots = append(handleSnapshots, snapshotWithTags)
		}
	}()

	// Start a goroutine to wait for all processing to complete
	go func() {
		wg.Wait()
		close(doneChan)
	}()

	for {
		select {
		case err := <-errorChan:
			// s.Logger.LogError("Error during snapshot processing", err, nil, false)
			return err
		case <-doneChan:
			defer close(tableRowChan)
			if err := handlerSnapshot(ctx, handleVolumesIDs, handleSnapshots, tableRowChan); err != nil {
				s.Logger.LogError("Error handling snapshots", err, nil, false)
				errorChan <- err
			}
			for row := range tableRowChan {
				tableRows = append(tableRows, *row)
			}

			// Print the volume table
			if err := printSnapshotTable(&tableRows); err != nil {
				s.Logger.LogError("Error printing volume table", err, nil, false)
				errorChan <- err
			}
			return nil
		case <-ctx.Done():
			s.Logger.LogInfo("Operation canceled", nil)
			return ctx.Err()
		}
	}
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

// describeSnapshots returns a list of snapshots owned by the account
func (s *AWSCommand) describeSnapshots(ctx context.Context, snapshotChan chan<- snapshotWithTags, filter []types.Filter) error {
	defer close(snapshotChan)

	// Create a paginator for Snapshots owned by self
	paginator := ec2.NewDescribeSnapshotsPaginator(s.AWSClient.EC2, &ec2.DescribeSnapshotsInput{
		MaxResults: aws.Int32(500),
		Filters:    filter,
	})

	// Iterate over the pages
	for paginator.HasMorePages() {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return err
			}

			// Send snapshots to the channel
			for _, snapshot := range page.Snapshots {
				// Convert tags to a map
				tagMap := utils.TagsToMap(snapshot.Tags)

				select {
				case snapshotChan <- snapshotWithTags{Snapshot: snapshot, TagMap: tagMap}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
	return nil
}

// handlerSnapshot processes the snapshots and volumes
func handlerSnapshot(ctx context.Context, handleVolumesIDs []volumeWithTags, handleSnapshots []snapshotWithTags, tableRowChan chan<- *table.Row) error {
	defer close(tableRowChan)

	// Create a map to store the volume IDs
	volumeSnapshotID := make(map[string]bool)

	// Create a slice to store the total size of all volumes
	size := int32(0)
	// Process volume IDs
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		for _, volumeWithTags := range handleVolumesIDs {
			snapshotID := volumeWithTags.Volume.SnapshotId
			if strings.Contains(*snapshotID, "snap") {
				volumeSnapshotID[*snapshotID] = true
			}
			volumeSnapshotID[*snapshotID] = false
		}

		for _, snapshotWithTags := range handleSnapshots {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				snapshotVolumeID := snapshotWithTags.Snapshot.VolumeId
				snapshotSize := snapshotWithTags.Snapshot.VolumeSize
				tagMap := snapshotWithTags.TagMap
				nameTag, ok := tagMap["Name"]
				if !ok {
					nameTag = types.Tag{Value: aws.String("-")}
				}

				if !strings.Contains(*snapshotWithTags.Snapshot.Description, "Created by CreateImage") || !strings.Contains(*snapshotWithTags.Snapshot.Description, "Created for policy") {
					if volumeSnapshotID[*snapshotWithTags.Snapshot.SnapshotId] {
						tableRowChan <- &table.Row{
							*nameTag.Value,
							*snapshotVolumeID,
							*snapshotSize,
						}
						size += *snapshotSize
					}
				}
			}
			tableRowChan <- &table.Row{"Total", "", size}
		}
	}
	return nil
}

// printSnapshotTable prints the snapshot table
func printSnapshotTable(tableRows *[]table.Row) error {

	columnConfig := []table.ColumnConfig{
		{
			Name:        "Name",
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

	printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"Name", "Snapshot ID", "Size"}, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}}, &columnConfig)

	return printerClient.PrintTextTable(tableRows)
}
