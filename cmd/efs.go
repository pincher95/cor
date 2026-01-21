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
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/efs/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
)

var efsCmd = &cobra.Command{
	Use:   "efs",
	Short: "List and optionally delete EFS file systems with zero mount targets",
	Long:  `List EFS file systems that have no mount targets (typically unused) and optionally delete them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout
		ctx := cmd.Context()

		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		additionalFlags := []flags.Flag{
			{Name: "filter-by-name", Type: "string"},
			{Name: "include-attached", Type: "bool"},
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

		efsClient := efs.NewFromConfig(*cfg)
		awsClient := &handlers.AWSClientImpl{EFS: efsClient}

		return runEFSCmd(ctx, prompterClient, output, awsClient, flagValues)
	},
}

func init() {
	efsCmd.Flags().String("filter-by-name", "", "Filter EFS by Name tag (empty = no filter).")
	efsCmd.Flags().Bool("include-attached", false, "Include file systems that have mount targets (default: show only orphans).")
}

func runEFSCmd(ctx context.Context, prompter prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  prompter,
		Output:    output,
	}
	return command.executeEFS(ctx, flagValues)
}

func (e *AWSCommand) executeEFS(ctx context.Context, flagValues *map[string]any) error {
	rootCtx := ctx
	collectDeletes := (*flagValues)["delete"].(bool)
	includeAttached := (*flagValues)["include-attached"].(bool)
	filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))

	stream := printer.NewStreamTable(e.Output, true, []string{"Name", "FileSystem ID", "MountTargets", "SizeBytes", "LifecycleState"})
	stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
	defer stream.Close()

	deleteIDs := make([]string, 0)
	paginator := efs.NewDescribeFileSystemsPaginator(e.AWSClient.EFS, &efs.DescribeFileSystemsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, fs := range page.FileSystems {
			mtCount := fs.NumberOfMountTargets
			if !includeAttached && mtCount > 0 {
				continue
			}

			name := "-"
			tagOutput, err := e.AWSClient.EFS.ListTagsForResource(ctx, &efs.ListTagsForResourceInput{
				ResourceId: fs.FileSystemId,
			})
			if err == nil {
				for _, tag := range tagOutput.Tags {
					if tag.Key != nil && *tag.Key == "Name" && tag.Value != nil {
						name = *tag.Value
						break
					}
				}
			}

			if filterByName != "" && !strings.Contains(name, filterByName) {
				continue
			}

			sizeBytes := fs.SizeInBytes.Value
			state := string(fs.LifeCycleState)
			if state == "" {
				state = string(types.LifeCycleStateAvailable)
			}

			stream.WriteRow(name, aws.ToString(fs.FileSystemId), mtCount, sizeBytes, state)
			if collectDeletes {
				deleteIDs = append(deleteIDs, aws.ToString(fs.FileSystemId))
			}
		}
	}

	if collectDeletes {
		if len(deleteIDs) == 0 {
			return nil
		}
		confirm, err := confirmDelete(e.Prompter, e.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, fsID := range deleteIDs {
			e.Logger.LogInfo("Deleting EFS", map[string]any{"FileSystemId": fsID})
			if _, err := e.AWSClient.EFS.DeleteFileSystem(rootCtx, &efs.DeleteFileSystemInput{FileSystemId: aws.String(fsID)}); err != nil {
				return err
			}
		}
	}

	return nil
}
