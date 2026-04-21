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
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	elasticachetypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

type orphanElastiCacheCluster struct {
	ClusterID         string
	Engine            string
	CacheNodeType     string
	NumNodes          int
	Status            string
	CreatedDate       string
	ZeroConnections   bool
	DaysSinceActivity int64
	Reason            string
}

// elasticacheCmd represents the elasticache command
var elasticacheCmd = &cobra.Command{
	Use:   "elasticache",
	Short: "List orphaned ElastiCache clusters",
	Long: `Finds ElastiCache clusters (Redis/Memcached) that are potentially orphaned based on:
- Clusters with zero active connections for extended periods (default: 24 hours)
- Clusters with no network traffic (bytes in/out near zero)

Orphaned ElastiCache clusters can incur significant costs:
- cache.m5.large: ~$105/month
- cache.r6g.xlarge: ~$217/month
- Extended support charges (80% premium) may apply for older engine versions`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "hours-zero-connections", Type: "int"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{
					ElastiCache: elasticache.NewFromConfig(*cfg),
					CloudWatch:  cloudwatch.NewFromConfig(*cfg),
				}
			},
		}, (*AWSCommand).executeElastiCache)
	},
}

func init() {
	elasticacheCmd.Flags().Int("hours-zero-connections", 24, "Consider clusters orphaned if zero connections for this many hours")
}

func (a *AWSCommand) executeElastiCache(ctx context.Context, flagValues *map[string]any) error {
	rootCtx := ctx
	collectDeletes := (*flagValues)["delete"].(bool)
	hoursZeroConnections := int64((*flagValues)["hours-zero-connections"].(int))

	clusterChan := make(chan *elasticachetypes.CacheCluster, 50)
	resultsChan := make(chan table.Row, 50)
	orphanClusters := []orphanElastiCacheCluster{}

	g, egCtx := errgroup.WithContext(ctx)

	// Goroutine to list all ElastiCache clusters
	g.Go(func() error {
		defer close(clusterChan)
		paginator := elasticache.NewDescribeCacheClustersPaginator(a.AWSClient.ElastiCache, &elasticache.DescribeCacheClustersInput{})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(egCtx)
			if err != nil {
				a.Logger.LogError("Error listing ElastiCache clusters", err, nil, false)
				return err
			}
			for i := range page.CacheClusters {
				select {
				case clusterChan <- &page.CacheClusters[i]:
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
				case cluster, ok := <-clusterChan:
					if !ok {
						return nil
					}

					orphan, err := a.checkElastiCacheOrphan(egCtx, cluster, hoursZeroConnections)
					if err != nil {
						a.Logger.LogError("Error checking ElastiCache cluster", err, map[string]any{
							"cluster": aws.ToString(cluster.CacheClusterId),
						}, false)
						continue
					}

					if orphan != nil {
						select {
						case resultsChan <- table.Row{
							orphan.ClusterID,
							orphan.Engine,
							orphan.CacheNodeType,
							orphan.NumNodes,
							orphan.Status,
							orphan.CreatedDate,
							fmt.Sprintf("%d days", orphan.DaysSinceActivity),
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
	headers := []string{"Cluster ID", "Engine", "Node Type", "Nodes", "Status", "Created", "Days Since Activity", "Reason"}

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

	a.Logger.LogInfo(fmt.Sprintf("Found %d orphaned ElastiCache clusters", len(orphanClusters)), nil)

	if collectDeletes && len(orphanClusters) > 0 {
		confirm, err := confirmDelete(a.Prompter, a.Logger)
		if err != nil || !confirm {
			return err
		}

		a.Logger.LogInfo("Deleting orphaned ElastiCache clusters...", nil)
		for _, cluster := range orphanClusters {
			if err := a.deleteElastiCacheCluster(rootCtx, cluster.ClusterID); err != nil {
				a.Logger.LogError("Failed to delete ElastiCache cluster", err, map[string]any{
					"cluster": cluster.ClusterID,
				}, false)
				return err
			}
			a.Logger.LogInfo(fmt.Sprintf("Deleted ElastiCache cluster: %s", cluster.ClusterID), nil)
		}
	}

	return nil
}

func (a *AWSCommand) checkElastiCacheOrphan(ctx context.Context, cluster *elasticachetypes.CacheCluster, hoursZeroConnections int64) (*orphanElastiCacheCluster, error) {
	clusterID := aws.ToString(cluster.CacheClusterId)

	// Check connections metric using CloudWatch
	endTime := time.Now()
	startTime := endTime.Add(-time.Duration(hoursZeroConnections) * time.Hour)

	// Check CurrConnections metric
	connectionsInput := &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/ElastiCache"),
		MetricName: aws.String("CurrConnections"),
		Dimensions: []cloudwatchtypes.Dimension{
			{
				Name:  aws.String("CacheClusterId"),
				Value: aws.String(clusterID),
			},
		},
		StartTime:  &startTime,
		EndTime:    &endTime,
		Period:     aws.Int32(3600), // 1 hour
		Statistics: []cloudwatchtypes.Statistic{cloudwatchtypes.StatisticAverage, cloudwatchtypes.StatisticMaximum},
	}

	connectionsOutput, err := a.AWSClient.CloudWatch.GetMetricStatistics(ctx, connectionsInput)
	if err != nil {
		return nil, err
	}

	// Check if cluster has had any connections
	hasConnections := false
	for _, datapoint := range connectionsOutput.Datapoints {
		if datapoint.Average != nil && *datapoint.Average > 0 {
			hasConnections = true
			break
		}
		if datapoint.Maximum != nil && *datapoint.Maximum > 0 {
			hasConnections = true
			break
		}
	}

	// Check network bytes in/out
	networkBytesInput := &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/ElastiCache"),
		MetricName: aws.String("NetworkBytesIn"),
		Dimensions: []cloudwatchtypes.Dimension{
			{
				Name:  aws.String("CacheClusterId"),
				Value: aws.String(clusterID),
			},
		},
		StartTime:  &startTime,
		EndTime:    &endTime,
		Period:     aws.Int32(3600),
		Statistics: []cloudwatchtypes.Statistic{cloudwatchtypes.StatisticSum},
	}

	networkOutput, err := a.AWSClient.CloudWatch.GetMetricStatistics(ctx, networkBytesInput)
	if err != nil {
		return nil, err
	}

	hasNetworkActivity := false
	for _, datapoint := range networkOutput.Datapoints {
		if datapoint.Sum != nil && *datapoint.Sum > 1000 { // More than 1KB
			hasNetworkActivity = true
			break
		}
	}

	// Calculate days since creation
	var daysSinceCreation int64
	if cluster.CacheClusterCreateTime != nil {
		daysSinceCreation = int64(time.Since(*cluster.CacheClusterCreateTime).Hours() / 24)
	}

	// Determine if orphaned
	reasons := []string{}
	if !hasConnections {
		reasons = append(reasons, fmt.Sprintf("Zero connections for %d+ hours", hoursZeroConnections))
	}
	if !hasNetworkActivity {
		reasons = append(reasons, "No significant network activity")
	}

	// Only flag as orphan if no connections and no network activity
	if len(reasons) < 2 {
		return nil, nil
	}

	createdDate := "N/A"
	if cluster.CacheClusterCreateTime != nil {
		createdDate = cluster.CacheClusterCreateTime.Format("2006-01-02")
	}

	numNodes := 0
	if cluster.NumCacheNodes != nil {
		numNodes = int(*cluster.NumCacheNodes)
	}

	return &orphanElastiCacheCluster{
		ClusterID:         clusterID,
		Engine:            aws.ToString(cluster.Engine),
		CacheNodeType:     aws.ToString(cluster.CacheNodeType),
		NumNodes:          numNodes,
		Status:            aws.ToString(cluster.CacheClusterStatus),
		CreatedDate:       createdDate,
		ZeroConnections:   !hasConnections,
		DaysSinceActivity: daysSinceCreation,
		Reason:            "Zero connections & no network activity",
	}, nil
}

func (a *AWSCommand) deleteElastiCacheCluster(ctx context.Context, clusterID string) error {
	_, err := a.AWSClient.ElastiCache.DeleteCacheCluster(ctx, &elasticache.DeleteCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
	})
	return err
}
