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
	"github.com/aws/aws-sdk-go-v2/service/efs"
	efstypes "github.com/aws/aws-sdk-go-v2/service/efs/types"
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanEFS struct {
	name         string
	id           string
	mountTargets int32
	sizeBytes    int64
	state        string
}

var efsCmd = &cobra.Command{
	Use:   "efs",
	Short: "List and optionally delete EFS file systems with zero mount targets",
	Long:  `List EFS file systems that have no mount targets (typically unused) and optionally delete them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "include-attached", Type: "bool"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{EFS: efs.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeEFS)
	},
}

func init() {
	efsCmd.Flags().String("filter-by-name", "", "Filter EFS by Name tag (empty = no filter).")
	efsCmd.Flags().Bool("include-attached", false, "Include file systems that have mount targets (default: show only orphans).")
}

func (a *AWSCommand) executeEFS(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByName := normalizeFilterValue((*extras)["filter-by-name"].(string))
	includeAttached := (*extras)["include-attached"].(bool)

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[efstypes.FileSystemDescription, orphanEFS]{
		Headers:       []string{"Name", "FileSystem ID", "MountTargets", "SizeBytes", "LifecycleState"},
		ResourceLabel: "EFS file systems",
		List: func(ctx context.Context, emit func(efstypes.FileSystemDescription) error) error {
			p := efs.NewDescribeFileSystemsPaginator(a.AWSClient.EFS, &efs.DescribeFileSystemsInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, fs := range page.FileSystems {
					if err := emit(fs); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, fs efstypes.FileSystemDescription) (*orphanEFS, error) {
			mtCount := fs.NumberOfMountTargets
			if !includeAttached && mtCount > 0 {
				return nil, nil
			}
			name := "-"
			if tagOutput, err := a.AWSClient.EFS.ListTagsForResource(ctx, &efs.ListTagsForResourceInput{
				ResourceId: fs.FileSystemId,
			}); err == nil {
				for _, tag := range tagOutput.Tags {
					if tag.Key != nil && *tag.Key == "Name" && tag.Value != nil {
						name = *tag.Value
						break
					}
				}
			}
			if !matchesFilterValue(name, filterByName) {
				return nil, nil
			}
			sizeBytes := int64(0)
			if fs.SizeInBytes != nil {
				sizeBytes = fs.SizeInBytes.Value
			}
			state := string(fs.LifeCycleState)
			if state == "" {
				state = string(efstypes.LifeCycleStateAvailable)
			}
			return &orphanEFS{
				name:         name,
				id:           aws.ToString(fs.FileSystemId),
				mountTargets: mtCount,
				sizeBytes:    sizeBytes,
				state:        state,
			}, nil
		},
		ToRow: func(r orphanEFS) []any {
			return []any{r.name, r.id, r.mountTargets, r.sizeBytes, r.state}
		},
		Delete: func(ctx context.Context, r orphanEFS) error {
			a.Logger.LogInfo("Deleting EFS", map[string]any{"FileSystemId": r.id})
			_, err := a.AWSClient.EFS.DeleteFileSystem(ctx, &efs.DeleteFileSystemInput{FileSystemId: aws.String(r.id)})
			return err
		},
		MonthlyCost: func(r orphanEFS) cost.USD {
			gb := float64(r.sizeBytes) / (1024 * 1024 * 1024)
			return cost.USD(gb) * a.Pricing.EFSStandardGB()
		},
	})
}
