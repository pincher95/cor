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
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
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

func (c *AWSCommand) executeRDS(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	// If neither flag is set, default to both true (preserves pre-runner behavior).
	if !(*extras)["include-instances"].(bool) && !(*extras)["include-snapshots"].(bool) {
		(*extras)["include-instances"] = true
		(*extras)["include-snapshots"] = true
	}
	rootCtx := ctx

	collectDeletes := globals.Delete
	deleteCandidates := make([]rdsResource, 0)

	resChan := make(chan rdsResource, 50)
	g, egCtx := errgroup.WithContext(ctx)
	var producerWG sync.WaitGroup

	// Producer: Instances
	if (*extras)["include-instances"].(bool) {
		producerWG.Add(1)
		g.Go(func() error {
			defer producerWG.Done()
			// Note: DescribeDBInstances does not support server-side Filters.
			paginator := rds.NewDescribeDBInstancesPaginator(c.AWSClient.RDS, &rds.DescribeDBInstancesInput{})
			for paginator.HasMorePages() {
				page, err := paginator.NextPage(egCtx)
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
					select {
					case <-egCtx.Done():
						return egCtx.Err()
					case resChan <- rdsResource{
						Type:    "Instance",
						ID:      aws.ToString(inst.DBInstanceIdentifier),
						Status:  aws.ToString(inst.DBInstanceStatus),
						Created: created,
					}:
					}
				}
			}
			return nil
		})
	}

	// Producer: Snapshots
	if (*extras)["include-snapshots"].(bool) {
		producerWG.Add(1)
		g.Go(func() error {
			defer producerWG.Done()
			paginator := rds.NewDescribeDBSnapshotsPaginator(c.AWSClient.RDS, &rds.DescribeDBSnapshotsInput{
				SnapshotType: aws.String("manual"),
			})
			for paginator.HasMorePages() {
				page, err := paginator.NextPage(egCtx)
				if err != nil {
					return err
				}
				for _, snap := range page.DBSnapshots {
					created := ""
					if snap.SnapshotCreateTime != nil {
						created = snap.SnapshotCreateTime.UTC().Format(time.RFC3339)
					}
					select {
					case <-egCtx.Done():
						return egCtx.Err()
					case resChan <- rdsResource{
						Type:    "Snapshot",
						ID:      aws.ToString(snap.DBSnapshotIdentifier),
						Status:  aws.ToString(snap.Status),
						Created: created,
					}:
					}
				}
			}
			return nil
		})
	}

	// Close channel when producers are done
	go func() {
		producerWG.Wait()
		close(resChan)
	}()

	// Stream output + delete as we go
	stream := printer.NewStreamTable(c.Output, true, []string{"Type", "ID", "Status", "Created"})
	stream.SetSort(globals.SortBy, globals.SortDesc)
	defer stream.Close()

	for res := range resChan {
		stream.WriteRow(res.Type, res.ID, res.Status, res.Created)

		if collectDeletes {
			deleteCandidates = append(deleteCandidates, res)
		}
	}

	if err := g.Wait(); err != nil {
		c.Logger.LogError("Error processing RDS resources", err, nil, false)
		return err
	}

	if collectDeletes {
		if len(deleteCandidates) == 0 {
			return nil
		}
		confirm, err := confirmDelete(c.Prompter, c.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, res := range deleteCandidates {
			if res.Type == "Instance" {
				c.Logger.LogInfo("Deleting RDS Instance (SkipFinalSnapshot=true)", map[string]any{"ID": res.ID})
				_, err := c.AWSClient.DeleteDBInstance(rootCtx, &rds.DeleteDBInstanceInput{
					DBInstanceIdentifier: aws.String(res.ID),
					SkipFinalSnapshot:    aws.Bool(true),
				})
				if err != nil {
					return err
				}
				continue
			}

			c.Logger.LogInfo("Deleting DB Snapshot", map[string]any{"ID": res.ID})
			_, err := c.AWSClient.DeleteDBSnapshot(rootCtx, &rds.DeleteDBSnapshotInput{
				DBSnapshotIdentifier: aws.String(res.ID),
			})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// Legacy pretty-table printer removed in favor of streaming output for low memory usage.
