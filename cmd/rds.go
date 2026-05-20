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
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
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
				{Name: "filter-by-name", Type: "string"},
				{Name: "creation-date-before", Type: "string"},
				{Name: "creation-date-after", Type: "string"},
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
	rdsCmd.Flags().String("filter-by-name", "", "Filter instance/snapshot IDs by Go regexp (RE2). For snapshots, also matches the source DB instance identifier. Anchor with ^…$ for exact match.")
	rdsCmd.Flags().String("creation-date-before", "", "Only include instances/snapshots created on or before this UTC date (YYYY-MM-DD).")
	rdsCmd.Flags().String("creation-date-after", "", "Only include instances/snapshots created on or after this UTC date (YYYY-MM-DD).")
}

type rdsKind string

const (
	rdsInstance rdsKind = "Instance"
	rdsSnapshot rdsKind = "Snapshot"
)

type rdsResource struct {
	Type    rdsKind
	ID      string
	Status  string
	Created string
	Class   string
	SizeGB  int32
}

func (a *AWSCommand) executeRDS(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	includeInstances := (*extras)["include-instances"].(bool)
	includeSnapshots := (*extras)["include-snapshots"].(bool)
	// Default both on when neither flag is set. Keep this local — extras
	// is shared across regional goroutines under --all-regions and writing
	// to it races with sibling regions.
	if !includeInstances && !includeSnapshots {
		includeInstances = true
		includeSnapshots = true
	}

	var nameRegex *regexp.Regexp
	if v, ok := (*extras)["filter-by-name"].(string); ok {
		if pat := strings.TrimSpace(v); pat != "" {
			re, err := regexp.Compile(pat)
			if err != nil {
				return fmt.Errorf("invalid --filter-by-name regex %q: %w", pat, err)
			}
			nameRegex = re
		}
	}
	idMatches := func(id string) bool {
		if nameRegex == nil {
			return true
		}
		return nameRegex.MatchString(id)
	}

	var beforeDate, afterDate *time.Time
	const dateLayout = "2006-01-02"
	if v, ok := (*extras)["creation-date-before"].(string); ok && strings.TrimSpace(v) != "" {
		t, err := time.Parse(dateLayout, strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("invalid --creation-date-before: %w", err)
		}
		beforeDate = &t
	}
	if v, ok := (*extras)["creation-date-after"].(string); ok && strings.TrimSpace(v) != "" {
		t, err := time.Parse(dateLayout, strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("invalid --creation-date-after: %w", err)
		}
		afterDate = &t
	}
	dateMatches := func(createdAt *time.Time) bool {
		// Missing timestamp short-circuits to "include" — never exclude a row
		// solely because AWS didn't surface a CreateTime field.
		if createdAt == nil {
			return true
		}
		if beforeDate != nil && createdAt.After(*beforeDate) {
			return false
		}
		if afterDate != nil && createdAt.Before(*afterDate) {
			return false
		}
		return true
	}

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
					id := aws.ToString(inst.DBInstanceIdentifier)
					if !idMatches(id) {
						continue
					}
					if !dateMatches(inst.InstanceCreateTime) {
						continue
					}
					created := ""
					if inst.InstanceCreateTime != nil {
						created = inst.InstanceCreateTime.UTC().Format(time.RFC3339)
					}
					if err := emit(rdsResource{
						Type:    rdsInstance,
						ID:      id,
						Status:  aws.ToString(inst.DBInstanceStatus),
						Created: created,
						Class:   aws.ToString(inst.DBInstanceClass),
						SizeGB:  aws.ToInt32(inst.AllocatedStorage),
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
					id := aws.ToString(snap.DBSnapshotIdentifier)
					// Snapshot IDs are often opaque (e.g. "rds:my-db-2024-01-01")
					// so match against either the snapshot's own ID or its source
					// DB instance identifier — users typically mean "snapshots of
					// DB X" when they filter snapshots by name.
					sourceID := aws.ToString(snap.DBInstanceIdentifier)
					if !idMatches(id) && !idMatches(sourceID) {
						continue
					}
					if !dateMatches(snap.SnapshotCreateTime) {
						continue
					}
					created := ""
					if snap.SnapshotCreateTime != nil {
						created = snap.SnapshotCreateTime.UTC().Format(time.RFC3339)
					}
					if err := emit(rdsResource{
						Type:    rdsSnapshot,
						ID:      id,
						Status:  aws.ToString(snap.Status),
						Created: created,
						SizeGB:  aws.ToInt32(snap.AllocatedStorage),
					}); err != nil {
						return err
					}
				}
			}
			return nil
		})
	}

	// RDS gives free manual-snapshot storage equal to the sum of running-
	// DB AllocatedStorage; snapshots only bill above that allowance.
	var freeAllowanceGB int64
	var snapshotConsumedGB atomic.Int64
	var totalSnapshotGB int64 // proration denominator under --with-ce

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[rdsResource, rdsResource]{
		Headers:       []string{"Type", "ID", "Status", "Created"},
		ResourceLabel: "RDS resources",
		PreScan: func(ctx context.Context) error {
			g, gctx := errgroup.WithContext(ctx)
			g.Go(func() error {
				p := rds.NewDescribeDBInstancesPaginator(a.AWSClient.RDS, &rds.DescribeDBInstancesInput{})
				for p.HasMorePages() {
					page, err := p.NextPage(gctx)
					if err != nil {
						return err
					}
					for _, inst := range page.DBInstances {
						freeAllowanceGB += int64(aws.ToInt32(inst.AllocatedStorage))
					}
				}
				return nil
			})
			if a.CEActuals != nil {
				g.Go(func() error {
					sp := rds.NewDescribeDBSnapshotsPaginator(a.AWSClient.RDS, &rds.DescribeDBSnapshotsInput{
						SnapshotType: aws.String("manual"),
					})
					for sp.HasMorePages() {
						page, err := sp.NextPage(gctx)
						if err != nil {
							return err
						}
						for _, snap := range page.DBSnapshots {
							totalSnapshotGB += int64(aws.ToInt32(snap.AllocatedStorage))
						}
					}
					return nil
				})
			}
			return g.Wait()
		},
		Lists: producers,
		Process: func(_ context.Context, r rdsResource) (*rdsResource, error) {
			return &r, nil
		},
		ToRow: func(r rdsResource) []any {
			return []any{r.Type, r.ID, r.Status, r.Created}
		},
		Delete: func(ctx context.Context, r rdsResource) error {
			switch r.Type {
			case rdsInstance:
				a.Logger.LogInfo("Deleting RDS Instance (SkipFinalSnapshot=true)", map[string]any{"ID": r.ID})
				_, err := a.AWSClient.RDS.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
					DBInstanceIdentifier: aws.String(r.ID),
					SkipFinalSnapshot:    aws.Bool(true),
				})
				return err
			case rdsSnapshot:
				a.Logger.LogInfo("Deleting DB Snapshot", map[string]any{"ID": r.ID})
				_, err := a.AWSClient.RDS.DeleteDBSnapshot(ctx, &rds.DeleteDBSnapshotInput{
					DBSnapshotIdentifier: aws.String(r.ID),
				})
				return err
			}
			return nil
		},
		MonthlyCost: func(r rdsResource) cost.USD {
			switch r.Type {
			case rdsInstance:
				// Stopped RDS bills storage only, not compute. AWS auto-
				// restarts after 7 days stopped — not modeled here.
				return cost.USD(float64(r.SizeGB)) * 0.115
			case rdsSnapshot:
				billable := int64(r.SizeGB)
				used := snapshotConsumedGB.Add(billable)
				remaining := used - freeAllowanceGB
				heuristic := cost.USD(0)
				if remaining > 0 {
					if remaining > billable {
						remaining = billable
					}
					heuristic = cost.USD(float64(remaining)) * a.Pricing.RDSManualSnapshotGB() * 0.4
				}
				if a.CEActuals == nil || a.CEActuals.RDSManualSnapshot == 0 {
					return heuristic
				}
				return cost.Prorate(a.CEActuals.RDSManualSnapshot, float64(r.SizeGB), float64(totalSnapshotGB), heuristic)
			}
			return 0
		},
	})
}
