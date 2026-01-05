/*
Copyright 2024 Elastic Scaler Contributors.

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
	"io"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var rdsCmd = &cobra.Command{
	Use:   "rds",
	Short: "Find stopped RDS instances and manual snapshots",
	Long:  `List and optionally delete stopped RDS instances and manual DB snapshots.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout
		ctx := cmd.Context()

		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		additionalFlags := []flags.Flag{
			{Name: "include-instances", Type: "bool"},
			{Name: "include-snapshots", Type: "bool"},
		}

		flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
		if err != nil {
			return err
		}

		// If neither flag is set, default to both true
		incInst := (*flagValues)["include-instances"].(bool)
		incSnap := (*flagValues)["include-snapshots"].(bool)
		if !incInst && !incSnap {
			(*flagValues)["include-instances"] = true
			(*flagValues)["include-snapshots"] = true
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

		rdsClient := rds.NewFromConfig(*cfg)

		awsClient := &handlers.AWSClientImpl{
			RDS: rdsClient,
		}

		return runRDSCmd(ctx, prompterClient, output, awsClient, flagValues)
	},
}

func init() {
	rdsCmd.Flags().Bool("include-instances", false, "Include stopped RDS instances")
	rdsCmd.Flags().Bool("include-snapshots", false, "Include manual RDS snapshots")
}

func runRDSCmd(ctx context.Context, prompter prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  prompter,
		Output:    output,
	}
	return command.executeRDS(ctx, flagValues)
}

type rdsResource struct {
	Type    string
	ID      string
	Status  string
	Created string
}

func (c *AWSCommand) executeRDS(ctx context.Context, flagValues *map[string]any) error {
	rootCtx := ctx

	// If deleting, confirm up-front so we can stream without buffering IDs.
	doDelete := false
	if (*flagValues)["delete"].(bool) {
		confirm, err := c.Prompter.Confirm("Are you sure you want to proceed? (yes/no): ")
		if err != nil {
			return err
		}
		if confirm == nil || !*confirm {
			c.Logger.LogInfo("Aborted.", nil)
			return nil
		}
		doDelete = true
	}

	resChan := make(chan rdsResource, 50)
	g, egCtx := errgroup.WithContext(ctx)
	var producerWG sync.WaitGroup

	// Producer: Instances
	if (*flagValues)["include-instances"].(bool) {
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
	if (*flagValues)["include-snapshots"].(bool) {
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
	stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
	defer stream.Close()

	for res := range resChan {
		stream.WriteRow(res.Type, res.ID, res.Status, res.Created)

		if doDelete {
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

	if err := g.Wait(); err != nil {
		c.Logger.LogError("Error processing RDS resources", err, nil, false)
		return err
	}

	return nil
}

// Legacy pretty-table printer removed in favor of streaming output for low memory usage.
