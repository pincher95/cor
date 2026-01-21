/*
Copyright 2024 Cloud Orphaned Resources Contributors.

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
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/pincher95/cor/pkg/utils"
	"github.com/spf13/cobra"
)

// snapshotsCmd represents the snapshots command
var snapshotsCmd = &cobra.Command{
	Use:   "snapshots",
	Short: "Return Snapshots not associated with AMI, Volumes or created by Lifecycle policy",
	Long:  ``,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Create prompter using the prompter package
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
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

		return runSnapshotCmd(ctx, prompterClient, output, *awsClient, flagValues)
	},
}

func runSnapshotCmd(ctx context.Context, prompter prompter.Client, output io.Writer, awsClient handlers.AWSClientImpl, flagValues *map[string]any) error {

	// Create a new logger and error handler
	logger := logging.NewLogger()

	// Create an instance of elbv2Command
	command := &AWSCommand{
		AWSClient: awsClient,
		Logger:    logger,
		Prompter:  prompter,
		Output:    output,
	}

	return command.executeSnapShot(ctx, flagValues)
}

func (s *AWSCommand) executeSnapShot(ctx context.Context, flagValues *map[string]any) error {
	collectDeletes := (*flagValues)["delete"].(bool)
	type snapshotDeleteCandidate struct {
		id   string
		name string
	}
	deleteCandidates := make([]snapshotDeleteCandidate, 0)

	// Precompute usage sets (needed to accurately filter orphan snapshots).
	usedByImages, err := s.collectSnapshotsUsedByImages(ctx)
	if err != nil {
		return err
	}

	usedByVolumes := make(map[string]bool, 1024)
	volPaginator := ec2.NewDescribeVolumesPaginator(s.AWSClient.EC2, &ec2.DescribeVolumesInput{})
	for volPaginator.HasMorePages() {
		page, err := volPaginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, vol := range page.Volumes {
			if vol.SnapshotId != nil {
				usedByVolumes[*vol.SnapshotId] = true
			}
		}
	}

	// Stream output
	stream := printer.NewStreamTable(s.Output, true, []string{"Name", "Snapshot ID", "Size"})
	stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
	defer stream.Close()

	var totalSize int32
	filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))
	snapFilters := []types.Filter{}
	if filterByName != "" {
		snapFilters = append(snapFilters, types.Filter{Name: aws.String("tag:Name"), Values: []string{filterByName}})
	}
	snapPaginator := ec2.NewDescribeSnapshotsPaginator(s.AWSClient.EC2, &ec2.DescribeSnapshotsInput{
		OwnerIds: []string{"self"},
		Filters:  snapFilters,
	})

	for snapPaginator.HasMorePages() {
		page, err := snapPaginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, snap := range page.Snapshots {
			if snap.SnapshotId == nil || snap.VolumeSize == nil {
				continue
			}
			snapshotID := *snap.SnapshotId

			desc := aws.ToString(snap.Description)
			if strings.Contains(desc, "Created by CreateImage") || strings.Contains(desc, "Created for policy") {
				continue
			}
			if usedByImages[snapshotID] || usedByVolumes[snapshotID] {
				continue
			}

			name := "-"
			if nameTag, ok := utils.TagsToMap(snap.Tags)["Name"]; ok && nameTag.Value != nil {
				name = *nameTag.Value
			}

			stream.WriteRow(name, snapshotID, *snap.VolumeSize)
			totalSize += *snap.VolumeSize

			if collectDeletes {
				deleteCandidates = append(deleteCandidates, snapshotDeleteCandidate{id: snapshotID, name: name})
			}
		}
	}

	// Total
	stream.WriteRow("Total", "", totalSize)

	if collectDeletes {
		if len(deleteCandidates) == 0 {
			return nil
		}
		confirm, err := confirmDelete(s.Prompter, s.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, candidate := range deleteCandidates {
			s.Logger.LogInfo("Deleting Snapshot", map[string]any{"SnapshotID": candidate.id, "Name": candidate.name})
			if _, err := s.AWSClient.EC2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(candidate.id)}); err != nil {
				s.Logger.LogError("Error deleting snapshot", err, map[string]any{"SnapshotID": candidate.id}, false)
				return err
			}
		}
	}
	return nil
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
	snapshotsCmd.Flags().String("filter-by-name", "", "Filter snapshots by tag:Name (empty = no filter).")
}

func (s *AWSCommand) collectSnapshotsUsedByImages(ctx context.Context) (map[string]bool, error) {
	used := make(map[string]bool)
	paginator := ec2.NewDescribeImagesPaginator(s.AWSClient.EC2, &ec2.DescribeImagesInput{
		Owners:            []string{"self"},
		IncludeDeprecated: aws.Bool(true),
		IncludeDisabled:   aws.Bool(true),
	})

	pages := 0
	images := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		pages++
		images += len(page.Images)
		if pages%25 == 0 {
			s.Logger.LogInfo("Scanning AMIs for snapshot usage…", map[string]any{"pages": pages, "images": images})
		}
		for _, image := range page.Images {
			for _, mapping := range image.BlockDeviceMappings {
				if mapping.Ebs != nil && mapping.Ebs.SnapshotId != nil {
					used[*mapping.Ebs.SnapshotId] = true
				}
			}
		}
	}

	return used, nil
}
