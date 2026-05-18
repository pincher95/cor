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
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/utils"
	"github.com/spf13/cobra"
)

type orphanECRImage struct {
	repository string
	digest     string
	tags       string
	pushedAt   string
	sizeBytes  int64
}

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

func (a *AWSCommand) executeECR(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByName := normalizeFilterValue((*extras)["filter-by-name"].(string))
	untaggedOnly := (*extras)["untagged-only"].(bool)
	olderThanDaysStr := strings.TrimSpace((*extras)["older-than-days"].(string))

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

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[orphanECRImage, orphanECRImage]{
		Headers:       []string{"Repository", "ImageDigest", "Tags", "PushedAt"},
		ResourceLabel: "ECR images",
		List: func(ctx context.Context, emit func(orphanECRImage) error) error {
			repoPaginator := ecr.NewDescribeRepositoriesPaginator(a.AWSClient.ECR, &ecr.DescribeRepositoriesInput{})
			for repoPaginator.HasMorePages() {
				repoPage, err := repoPaginator.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, repo := range repoPage.Repositories {
					repoName := aws.ToString(repo.RepositoryName)
					if !matchesFilterValue(repoName, filterByName) {
						continue
					}
					filter := &ecrtypes.DescribeImagesFilter{TagStatus: ecrtypes.TagStatusAny}
					if cutoff.IsZero() && untaggedOnly {
						filter.TagStatus = ecrtypes.TagStatusUntagged
					}
					imgPaginator := ecr.NewDescribeImagesPaginator(a.AWSClient.ECR, &ecr.DescribeImagesInput{
						RepositoryName: aws.String(repoName),
						Filter:         filter,
					})
					for imgPaginator.HasMorePages() {
						imgPage, err := imgPaginator.NextPage(ctx)
						if err != nil {
							return err
						}
						for _, img := range imgPage.ImageDetails {
							include := false
							if untaggedOnly && len(img.ImageTags) == 0 {
								include = true
							}
							if !cutoff.IsZero() && img.ImagePushedAt != nil && img.ImagePushedAt.Before(cutoff) {
								include = true
							}
							if !include {
								continue
							}
							tagStr := "-"
							if len(img.ImageTags) > 0 {
								tagStr = strings.Join(img.ImageTags, ",")
							}
							pushedStr := "-"
							if img.ImagePushedAt != nil {
								pushedStr = img.ImagePushedAt.UTC().Format(time.RFC3339)
							}
							digest := aws.ToString(img.ImageDigest)
							if digest == "" {
								continue
							}
							if err := emit(orphanECRImage{
								repository: repoName,
								digest:     digest,
								tags:       tagStr,
								pushedAt:   pushedStr,
								sizeBytes:  aws.ToInt64(img.ImageSizeInBytes),
							}); err != nil {
								return err
							}
						}
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, img orphanECRImage) (*orphanECRImage, error) {
			return &img, nil
		},
		ToRow: func(r orphanECRImage) []any {
			return []any{r.repository, r.digest, r.tags, r.pushedAt}
		},
		MonthlyCost: func(r orphanECRImage) cost.USD {
			gb := float64(r.sizeBytes) / (1024 * 1024 * 1024)
			return cost.USD(gb) * a.Pricing.ECRGB()
		},
		DeleteBatch: func(ctx context.Context, rs []orphanECRImage) error {
			byRepo := make(map[string][]ecrtypes.ImageIdentifier, 8)
			for _, r := range rs {
				byRepo[r.repository] = append(byRepo[r.repository], ecrtypes.ImageIdentifier{
					ImageDigest: aws.String(r.digest),
				})
			}
			for repoName, ids := range byRepo {
				for _, chunk := range utils.SliceChunkBy(ids, 100) {
					if len(chunk) == 0 {
						continue
					}
					a.Logger.LogInfo("Deleting ECR images", map[string]any{"Repository": repoName, "count": len(chunk)})
					out, err := a.AWSClient.ECR.BatchDeleteImage(ctx, &ecr.BatchDeleteImageInput{
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
			return nil
		},
	})
}
