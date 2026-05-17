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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanLogGroup struct {
	name      string
	storedStr string
	retention string
}

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "List and optionally delete CloudWatch Log Groups",
	Long:  `List CloudWatch Log Groups. Useful for finding old or unused log groups storing data indefinitely.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{CWL: cloudwatchlogs.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeLogs)
	},
}

func init() {
	logsCmd.Flags().String("filter-by-name", "", "Filter log groups by name prefix")
}

func (a *AWSCommand) executeLogs(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	prefix := ""
	if p, ok := (*extras)["filter-by-name"].(string); ok {
		prefix = p
	}

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[cwltypes.LogGroup, orphanLogGroup]{
		Headers:       []string{"Log Group Name", "Stored Bytes", "Retention"},
		ResourceLabel: "log groups",
		List: func(ctx context.Context, emit func(cwltypes.LogGroup) error) error {
			input := &cloudwatchlogs.DescribeLogGroupsInput{}
			if prefix != "" {
				input.LogGroupNamePrefix = aws.String(prefix)
			}
			p := cloudwatchlogs.NewDescribeLogGroupsPaginator(a.AWSClient.CWL, input)
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, lg := range page.LogGroups {
					if err := emit(lg); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, lg cwltypes.LogGroup) (*orphanLogGroup, error) {
			retention := "Never Expire"
			if lg.RetentionInDays != nil {
				retention = fmt.Sprintf("Expires in %d days", *lg.RetentionInDays)
			}
			stored := int64(0)
			if lg.StoredBytes != nil {
				stored = *lg.StoredBytes
			}
			storedStr := fmt.Sprintf("%d B", stored)
			if stored > 1024*1024 {
				storedStr = fmt.Sprintf("%d MB", stored/(1024*1024))
			}
			return &orphanLogGroup{
				name:      aws.ToString(lg.LogGroupName),
				storedStr: storedStr,
				retention: retention,
			}, nil
		},
		ToRow: func(r orphanLogGroup) []any {
			return []any{r.name, r.storedStr, r.retention}
		},
		Delete: func(ctx context.Context, r orphanLogGroup) error {
			a.Logger.LogInfo("Deleting Log Group", map[string]any{"Name": r.name})
			_, err := a.AWSClient.CWL.DeleteLogGroup(ctx, &cloudwatchlogs.DeleteLogGroupInput{
				LogGroupName: aws.String(r.name),
			})
			return err
		},
		DeleteConcurrency: 10,
	})
}
