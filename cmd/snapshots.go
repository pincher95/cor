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
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

type orphanSnapshot struct {
	name string
	id   string
	size int32
}

var snapshotsCmd = &cobra.Command{
	Use:   "snapshots",
	Short: "Return Snapshots not associated with AMI, Volumes or created by Lifecycle policy",
	Long:  ``,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeSnapShot)
	},
}

func init() {
	snapshotsCmd.Flags().String("filter-by-name", "", "Filter snapshots by tag:Name (empty = no filter).")
}

func (a *AWSCommand) executeSnapShot(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByName := normalizeFilterValue((*extras)["filter-by-name"].(string))

	var usedByImages, usedByVolumes map[string]bool

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[types.Snapshot, orphanSnapshot]{
		Headers:       []string{"Name", "Snapshot ID", "Size"},
		ResourceLabel: "snapshots",
		PreScan: func(ctx context.Context) error {
			g, gctx := errgroup.WithContext(ctx)
			g.Go(func() error {
				m, err := a.collectSnapshotsUsedByImages(gctx)
				if err != nil {
					return err
				}
				usedByImages = m
				return nil
			})
			g.Go(func() error {
				m, err := a.collectSnapshotsUsedByVolumes(gctx)
				if err != nil {
					return err
				}
				usedByVolumes = m
				return nil
			})
			return g.Wait()
		},
		List: func(ctx context.Context, emit func(types.Snapshot) error) error {
			snapFilters := []types.Filter{}
			if filterByName != "" {
				snapFilters = append(snapFilters, types.Filter{
					Name:   aws.String("tag:Name"),
					Values: []string{filterByName},
				})
			}
			p := ec2.NewDescribeSnapshotsPaginator(a.AWSClient.EC2, &ec2.DescribeSnapshotsInput{
				OwnerIds: []string{"self"},
				Filters:  snapFilters,
			})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, snap := range page.Snapshots {
					if err := emit(snap); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, snap types.Snapshot) (*orphanSnapshot, error) {
			id := aws.ToString(snap.SnapshotId)
			if id == "" {
				return nil, nil
			}
			desc := aws.ToString(snap.Description)
			if strings.Contains(desc, "Created by CreateImage") || strings.Contains(desc, "Created for policy") {
				return nil, nil
			}
			if usedByImages[id] || usedByVolumes[id] {
				return nil, nil
			}
			name := ec2NameTag(snap.Tags)
			if name == "" {
				name = "-"
			}
			return &orphanSnapshot{name: name, id: id, size: aws.ToInt32(snap.VolumeSize)}, nil
		},
		ToRow: func(r orphanSnapshot) []any {
			return []any{r.name, r.id, r.size}
		},
		Finalize: func(rs []orphanSnapshot) []any {
			var total int32
			for _, r := range rs {
				total += r.size
			}
			return []any{"Total", "", total}
		},
		Delete: func(ctx context.Context, r orphanSnapshot) error {
			a.Logger.LogInfo("Deleting Snapshot", map[string]any{"SnapshotID": r.id, "Name": r.name})
			_, err := a.AWSClient.EC2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(r.id)})
			return err
		},
		DeleteConcurrency: 5,
		DedupKey:          func(r orphanSnapshot) string { return r.id },
	})
}

func (a *AWSCommand) collectSnapshotsUsedByImages(ctx context.Context) (map[string]bool, error) {
	used := make(map[string]bool)
	paginator := ec2.NewDescribeImagesPaginator(a.AWSClient.EC2, &ec2.DescribeImagesInput{
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
			a.Logger.LogInfo("Scanning AMIs for snapshot usage", map[string]any{"pages": pages, "images": images})
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

func (a *AWSCommand) collectSnapshotsUsedByVolumes(ctx context.Context) (map[string]bool, error) {
	used := make(map[string]bool, 1024)
	p := ec2.NewDescribeVolumesPaginator(a.AWSClient.EC2, &ec2.DescribeVolumesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, vol := range page.Volumes {
			if vol.SnapshotId != nil {
				used[*vol.SnapshotId] = true
			}
		}
	}
	return used, nil
}
