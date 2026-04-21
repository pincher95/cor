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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type volumePipelineResult struct {
	name, id, snapshotID string
	size                 int32
}

// volumesListCmd represents the volumes command
var volumesCmd = &cobra.Command{
	Use:   "volumes",
	Short: "List and optionally delete unattached EBS volumes",
	Long:  `List EBS volumes in 'available' state (not attached to any instance) and optionally delete them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeVolumes)
	},
}

func (v *AWSCommand) executeVolumes(ctx context.Context, flagValues *map[string]any) error {
	filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))

	return runOrphanPipeline(v, ctx, flagValues, OrphanPipeline[types.Volume, volumePipelineResult]{
		Headers: []string{"Name", "Volume ID", "Snapshot ID", "Size"},
		List: func(ctx context.Context, emit func(types.Volume) error) error {
			filters := []types.Filter{
				{Name: aws.String("status"), Values: []string{"available"}},
			}
			if filterByName != "" {
				filters = append(filters, types.Filter{
					Name:   aws.String("tag:Name"),
					Values: []string{filterByName},
				})
			}
			p := ec2.NewDescribeVolumesPaginator(v.AWSClient.EC2, &ec2.DescribeVolumesInput{Filters: filters})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, item := range page.Volumes {
					if err := emit(item); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, vol types.Volume) (*volumePipelineResult, error) {
			name := "-"
			for _, t := range vol.Tags {
				if aws.ToString(t.Key) == "Name" && t.Value != nil {
					name = *t.Value
					break
				}
			}
			return &volumePipelineResult{
				name:       name,
				id:         aws.ToString(vol.VolumeId),
				snapshotID: aws.ToString(vol.SnapshotId),
				size:       aws.ToInt32(vol.Size),
			}, nil
		},
		ToRow: func(r volumePipelineResult) []any {
			return []any{r.name, r.id, r.snapshotID, r.size}
		},
		Finalize: func(results []volumePipelineResult) []any {
			var total int32
			for _, r := range results {
				total += r.size
			}
			return []any{"Total", "", "", total}
		},
		Delete: func(ctx context.Context, r volumePipelineResult) error {
			v.Logger.LogInfo("Deleting Volume", map[string]any{"VolumeId": r.id})
			_, err := v.AWSClient.EC2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(r.id)})
			return err
		},
	})
}

func init() {
	volumesCmd.Flags().String("filter-by-name", "", "Filter volumes by tag:Name (empty = no filter).")
}
