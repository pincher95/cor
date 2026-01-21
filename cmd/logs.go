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
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "List and optionally delete CloudWatch Log Groups",
	Long:  `List CloudWatch Log Groups. Useful for finding old or unused log groups storing data indefinitely.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout
		ctx := cmd.Context()

		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		additionalFlags := []flags.Flag{
			{Name: "filter-by-name", Type: "string"},
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

		cwlClient := cloudwatchlogs.NewFromConfig(*cfg)

		awsClient := &handlers.AWSClientImpl{
			CWL: cwlClient,
		}

		return runLogsCmd(ctx, prompterClient, output, awsClient, flagValues)
	},
}

func init() {
	logsCmd.Flags().String("filter-by-name", "", "Filter log groups by name prefix")
}

func runLogsCmd(ctx context.Context, prompter prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  prompter,
		Output:    output,
	}
	return command.executeLogs(ctx, flagValues)
}

type logGroupRes struct {
	Name      string
	Stored    int64
	Retention string
}

func (c *AWSCommand) executeLogs(ctx context.Context, flagValues *map[string]any) error {
	collectDeletes := (*flagValues)["delete"].(bool)
	deleteNames := make([]string, 0)

	resChan := make(chan logGroupRes, 100)
	g, ctx := errgroup.WithContext(ctx)

	// Producer
	g.Go(func() error {
		defer close(resChan)
		input := &cloudwatchlogs.DescribeLogGroupsInput{}
		if prefix, ok := (*flagValues)["filter-by-name"].(string); ok && prefix != "" {
			input.LogGroupNamePrefix = aws.String(prefix)
		}

		paginator := cloudwatchlogs.NewDescribeLogGroupsPaginator(c.AWSClient.CWL, input)
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return err
			}
			for _, lg := range page.LogGroups {
				retention := "Never Expire"
				if lg.RetentionInDays != nil {
					retention = fmt.Sprintf("Expires in %d days", *lg.RetentionInDays)
				}
				stored := int64(0)
				if lg.StoredBytes != nil {
					stored = *lg.StoredBytes
				}

				select {
				case <-ctx.Done():
					return ctx.Err()
				case resChan <- logGroupRes{
					Name:      aws.ToString(lg.LogGroupName),
					Stored:    stored,
					Retention: retention,
				}:
				}
			}
		}
		return nil
	})

	stream := printer.NewStreamTable(c.Output, true, []string{"Log Group Name", "Stored Bytes", "Retention"})
	stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
	defer stream.Close()

	for res := range resChan {
		sizeStr := fmt.Sprintf("%d B", res.Stored)
		if res.Stored > 1024*1024 {
			sizeStr = fmt.Sprintf("%d MB", res.Stored/(1024*1024))
		}
		stream.WriteRow(res.Name, sizeStr, res.Retention)

		if collectDeletes {
			deleteNames = append(deleteNames, res.Name)
		}
	}

	if err := g.Wait(); err != nil {
		c.Logger.LogError("Error processing Log Groups", err, nil, false)
		return err
	}

	if collectDeletes {
		if len(deleteNames) == 0 {
			return nil
		}
		confirm, err := confirmDelete(c.Prompter, c.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, name := range deleteNames {
			c.Logger.LogInfo("Deleting Log Group", map[string]any{"Name": name})
			_, err := c.AWSClient.DeleteLogGroup(ctx, &cloudwatchlogs.DeleteLogGroupInput{
				LogGroupName: aws.String(name),
			})
			if err != nil {
				return err
			}
		}
	}

	return nil
}
