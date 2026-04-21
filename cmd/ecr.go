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
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/utils"
	"github.com/spf13/cobra"
)

var ecrCmd = &cobra.Command{
	Use:   "ecr",
	Short: "List and optionally delete untagged/old ECR images",
	Long:  `List ECR images that are untagged and/or older than a threshold, and optionally delete them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "untagged-only", Type: "bool"},
				{Name: "older-than-days", Type: "string"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{ECR: ecr.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeECR)
	},
}

func init() {
	ecrCmd.Flags().String("filter-by-name", "", "Filter by ECR repository name (substring match).")
	ecrCmd.Flags().Bool("untagged-only", true, "Include untagged images.")
	ecrCmd.Flags().String("older-than-days", "", "Include images older than N days (e.g. 30).")
}

func (e *AWSCommand) executeECR(ctx context.Context, flagValues *map[string]any) error {
	rootCtx := ctx
	collectDeletes := (*flagValues)["delete"].(bool)
	filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))
	untaggedOnly := (*flagValues)["untagged-only"].(bool)
	olderThanDaysStr := strings.TrimSpace((*flagValues)["older-than-days"].(string))

	var cutoff time.Time
	if olderThanDaysStr != "" {
		days, err := strconv.Atoi(olderThanDaysStr)
		if err != nil {
			return fmt.Errorf("invalid older-than-days: %w", err)
		}
		if days > 0 {
			cutoff = time.Now().AddDate(0, 0, -days)
		}
	}

	stream := printer.NewStreamTable(e.Output, true, []string{"Repository", "ImageDigest", "Tags", "PushedAt"})
	stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
	defer stream.Close()

	deleteMap := make(map[string][]ecrtypes.ImageIdentifier)

	repoPaginator := ecr.NewDescribeRepositoriesPaginator(e.AWSClient.ECR, &ecr.DescribeRepositoriesInput{})
	for repoPaginator.HasMorePages() {
		repoPage, err := repoPaginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, repo := range repoPage.Repositories {
			repoName := aws.ToString(repo.RepositoryName)
			if filterByName != "" && !strings.Contains(repoName, filterByName) {
				continue
			}

			filter := &ecrtypes.DescribeImagesFilter{TagStatus: ecrtypes.TagStatusAny}
			if cutoff.IsZero() && untaggedOnly {
				filter.TagStatus = ecrtypes.TagStatusUntagged
			}

			imgPaginator := ecr.NewDescribeImagesPaginator(e.AWSClient.ECR, &ecr.DescribeImagesInput{
				RepositoryName: aws.String(repoName),
				Filter:         filter,
			})

			for imgPaginator.HasMorePages() {
				imgPage, err := imgPaginator.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, img := range imgPage.ImageDetails {
					tags := img.ImageTags
					pushedAt := img.ImagePushedAt

					include := false
					if untaggedOnly && len(tags) == 0 {
						include = true
					}
					if !cutoff.IsZero() && pushedAt != nil && pushedAt.Before(cutoff) {
						include = true
					}

					if !include {
						continue
					}

					tagStr := "-"
					if len(tags) > 0 {
						tagStr = strings.Join(tags, ",")
					}
					pushedStr := "-"
					if pushedAt != nil {
						pushedStr = pushedAt.UTC().Format(time.RFC3339)
					}

					digest := aws.ToString(img.ImageDigest)
					stream.WriteRow(repoName, digest, tagStr, pushedStr)

					if collectDeletes && digest != "" {
						deleteMap[repoName] = append(deleteMap[repoName], ecrtypes.ImageIdentifier{
							ImageDigest: aws.String(digest),
						})
					}
				}
			}
		}
	}

	if collectDeletes {
		if len(deleteMap) == 0 {
			return nil
		}
		confirm, err := confirmDelete(e.Prompter, e.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}

		for repoName, ids := range deleteMap {
			for _, chunk := range utils.SliceChunkBy(ids, 100) {
				if len(chunk) == 0 {
					continue
				}
				out, err := e.AWSClient.ECR.BatchDeleteImage(rootCtx, &ecr.BatchDeleteImageInput{
					RepositoryName: aws.String(repoName),
					ImageIds:       chunk,
				})
				if err != nil {
					return err
				}
				if len(out.Failures) > 0 {
					return fmt.Errorf("failed to delete %d image(s) from %s", len(out.Failures), repoName)
				}
			}
		}
	}

	return nil
}
