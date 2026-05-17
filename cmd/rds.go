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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

var rdsCmd = &cobra.Command{
	Use:   "rds",
	Short: "Find stopped RDS instances and manual snapshots",
	Long:  `List and optionally delete stopped RDS instances and manual DB snapshots.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "include-instances", Type: "bool"},
				{Name: "include-snapshots", Type: "bool"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{RDS: rds.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeRDS)
	},
}

func init() {
	rdsCmd.Flags().Bool("include-instances", false, "Include stopped RDS instances")
	rdsCmd.Flags().Bool("include-snapshots", false, "Include manual RDS snapshots")
}

type rdsResource struct {
	Type    string
	ID      string
	Status  string
	Created string
}

func (a *AWSCommand) executeRDS(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	if !(*extras)["include-instances"].(bool) && !(*extras)["include-snapshots"].(bool) {
		(*extras)["include-instances"] = true
		(*extras)["include-snapshots"] = true
	}
	includeInstances := (*extras)["include-instances"].(bool)
	includeSnapshots := (*extras)["include-snapshots"].(bool)

	producers := []func(context.Context, func(rdsResource) error) error{}
	if includeInstances {
		producers = append(producers, func(ctx context.Context, emit func(rdsResource) error) error {
			p := rds.NewDescribeDBInstancesPaginator(a.AWSClient.RDS, &rds.DescribeDBInstancesInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, inst := range page.DBInstances {
					if aws.ToString(inst.DBInstanceStatus) != "stopped" {
						continue
					}
					created := ""
					if inst.InstanceCreateTime != nil {
						created = inst.InstanceCreateTime.UTC().Format(time.RFC3339)
					}
					if err := emit(rdsResource{
						Type:    "Instance",
						ID:      aws.ToString(inst.DBInstanceIdentifier),
						Status:  aws.ToString(inst.DBInstanceStatus),
						Created: created,
					}); err != nil {
						return err
					}
				}
			}
			return nil
		})
	}
	if includeSnapshots {
		producers = append(producers, func(ctx context.Context, emit func(rdsResource) error) error {
			p := rds.NewDescribeDBSnapshotsPaginator(a.AWSClient.RDS, &rds.DescribeDBSnapshotsInput{
				SnapshotType: aws.String("manual"),
			})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, snap := range page.DBSnapshots {
					created := ""
					if snap.SnapshotCreateTime != nil {
						created = snap.SnapshotCreateTime.UTC().Format(time.RFC3339)
					}
					if err := emit(rdsResource{
						Type:    "Snapshot",
						ID:      aws.ToString(snap.DBSnapshotIdentifier),
						Status:  aws.ToString(snap.Status),
						Created: created,
					}); err != nil {
						return err
					}
				}
			}
			return nil
		})
	}

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[rdsResource, rdsResource]{
		Headers:       []string{"Type", "ID", "Status", "Created"},
		ResourceLabel: "RDS resources",
		Lists:         producers,
		Process: func(_ context.Context, r rdsResource) (*rdsResource, error) {
			return &r, nil
		},
		ToRow: func(r rdsResource) []any {
			return []any{r.Type, r.ID, r.Status, r.Created}
		},
		Delete: func(ctx context.Context, r rdsResource) error {
			switch r.Type {
			case "Instance":
				a.Logger.LogInfo("Deleting RDS Instance (SkipFinalSnapshot=true)", map[string]any{"ID": r.ID})
				_, err := a.AWSClient.RDS.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
					DBInstanceIdentifier: aws.String(r.ID),
					SkipFinalSnapshot:    aws.Bool(true),
				})
				return err
			case "Snapshot":
				a.Logger.LogInfo("Deleting DB Snapshot", map[string]any{"ID": r.ID})
				_, err := a.AWSClient.RDS.DeleteDBSnapshot(ctx, &rds.DeleteDBSnapshotInput{
					DBSnapshotIdentifier: aws.String(r.ID),
				})
				return err
			}
			return nil
		},
	})
}
