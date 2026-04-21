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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

type orphanECSCluster struct {
	ClusterName     string
	ClusterARN      string
	Status          string
	RegisteredTasks int32
	RunningTasks    int32
	ServicesCount   int32
	Reason          string
}

// ecsCmd represents the ecs command
var ecsCmd = &cobra.Command{
	Use:   "ecs",
	Short: "List orphaned ECS clusters",
	Long: `Finds ECS clusters that are potentially orphaned based on:
- Clusters with zero services
- Clusters with zero running tasks
- Clusters with services scaled to zero

Orphaned ECS clusters prevent cleanup of related resources:
- Cluster itself is free, but often has attached resources
- NAT Gateway, ALB, and other infrastructure remain
- Can indicate abandoned infrastructure`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{ECS: ecs.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeECS)
	},
}

func init() {
	// No additional flags for ECS command
}

func (a *AWSCommand) executeECS(ctx context.Context, flagValues *map[string]any) error {
	rootCtx := ctx
	collectDeletes := (*flagValues)["delete"].(bool)

	clusterARNChan := make(chan string, 50)
	resultsChan := make(chan table.Row, 50)
	orphanClusters := []orphanECSCluster{}

	g, egCtx := errgroup.WithContext(ctx)

	// Goroutine to list all ECS clusters
	g.Go(func() error {
		defer close(clusterARNChan)
		paginator := ecs.NewListClustersPaginator(a.AWSClient.ECS, &ecs.ListClustersInput{})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(egCtx)
			if err != nil {
				a.Logger.LogError("Error listing ECS clusters", err, nil, false)
				return err
			}
			for _, clusterARN := range page.ClusterArns {
				select {
				case clusterARNChan <- clusterARN:
				case <-egCtx.Done():
					return egCtx.Err()
				}
			}
		}
		return nil
	})

	// Worker goroutines to check each cluster
	numWorkers := NumGoroutines
	for range numWorkers {
		g.Go(func() error {
			for {
				select {
				case <-egCtx.Done():
					return nil
				case clusterARN, ok := <-clusterARNChan:
					if !ok {
						return nil
					}

					orphan, err := a.checkECSOrphan(egCtx, clusterARN)
					if err != nil {
						a.Logger.LogError("Error checking ECS cluster", err, map[string]any{
							"cluster": clusterARN,
						}, false)
						continue
					}

					if orphan != nil {
						select {
						case resultsChan <- table.Row{
							orphan.ClusterName,
							orphan.Status,
							orphan.RegisteredTasks,
							orphan.RunningTasks,
							orphan.ServicesCount,
							orphan.Reason,
						}:
						case <-egCtx.Done():
							return egCtx.Err()
						}
						orphanClusters = append(orphanClusters, *orphan)
					}
				}
			}
		})
	}

	// Goroutine to collect results and print
	headers := []string{"Cluster Name", "Status", "Registered Tasks", "Running Tasks", "Services", "Reason"}

	t := printer.NewStreamTable(a.Output, false, headers)
	defer t.Close()

	go func() {
		for row := range resultsChan {
			t.WriteRow(row...)
		}
	}()

	if err := g.Wait(); err != nil {
		close(resultsChan)
		return err
	}
	close(resultsChan)
	time.Sleep(100 * time.Millisecond) // Give goroutine time to finish writing

	a.Logger.LogInfo(fmt.Sprintf("Found %d orphaned ECS clusters", len(orphanClusters)), nil)

	if collectDeletes && len(orphanClusters) > 0 {
		confirm, err := confirmDelete(a.Prompter, a.Logger)
		if err != nil || !confirm {
			return err
		}

		a.Logger.LogInfo("Deleting orphaned ECS clusters...", nil)
		for _, cluster := range orphanClusters {
			if err := a.deleteECSCluster(rootCtx, cluster.ClusterARN); err != nil {
				a.Logger.LogError("Failed to delete ECS cluster", err, map[string]any{
					"cluster": cluster.ClusterName,
				}, false)
				return err
			}
			a.Logger.LogInfo(fmt.Sprintf("Deleted ECS cluster: %s", cluster.ClusterName), nil)
		}
	}

	return nil
}

func (a *AWSCommand) checkECSOrphan(ctx context.Context, clusterARN string) (*orphanECSCluster, error) {
	// Describe cluster to get details
	describeOutput, err := a.AWSClient.ECS.DescribeClusters(ctx, &ecs.DescribeClustersInput{
		Clusters: []string{clusterARN},
	})
	if err != nil {
		return nil, err
	}

	if len(describeOutput.Clusters) == 0 {
		return nil, nil
	}

	cluster := describeOutput.Clusters[0]

	registeredTasks := cluster.RegisteredContainerInstancesCount
	runningTasks := cluster.RunningTasksCount
	activeServices := cluster.ActiveServicesCount

	// Check if cluster is orphaned (no services and no running tasks)
	if activeServices > 0 || runningTasks > 0 {
		return nil, nil
	}

	// Extract cluster name from ARN
	clusterName := aws.ToString(cluster.ClusterName)
	if clusterName == "" {
		// Parse from ARN if name not provided
		// ARN format: arn:aws:ecs:region:account-id:cluster/cluster-name
		parts := []rune(clusterARN)
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] == '/' {
				clusterName = string(parts[i+1:])
				break
			}
		}
	}

	reason := ""
	if activeServices == 0 && runningTasks == 0 {
		reason = "No services and no running tasks"
	} else if activeServices == 0 {
		reason = "No services"
	} else {
		reason = "No running tasks"
	}

	return &orphanECSCluster{
		ClusterName:     clusterName,
		ClusterARN:      clusterARN,
		Status:          aws.ToString(cluster.Status),
		RegisteredTasks: registeredTasks,
		RunningTasks:    runningTasks,
		ServicesCount:   activeServices,
		Reason:          reason,
	}, nil
}

func (a *AWSCommand) deleteECSCluster(ctx context.Context, clusterARN string) error {
	_, err := a.AWSClient.ECS.DeleteCluster(ctx, &ecs.DeleteClusterInput{
		Cluster: aws.String(clusterARN),
	})
	return err
}
