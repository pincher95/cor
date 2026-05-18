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
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
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

func (a *AWSCommand) executeElastiCache(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	hoursZeroConnections := int64((*extras)["hours-zero-connections"].(int))

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[elasticachetypes.CacheCluster, orphanElastiCacheCluster]{
		Headers:       []string{"Cluster ID", "Engine", "Node Type", "Nodes", "Status", "Created", "Days Since Activity", "Reason"},
		ResourceLabel: "ElastiCache clusters",
		HideIndex:     true,
		List: func(ctx context.Context, emit func(elasticachetypes.CacheCluster) error) error {
			p := elasticache.NewDescribeCacheClustersPaginator(a.AWSClient.ElastiCache, &elasticache.DescribeCacheClustersInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, c := range page.CacheClusters {
					if err := emit(c); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, c elasticachetypes.CacheCluster) (*orphanElastiCacheCluster, error) {
			orphan, err := a.checkElastiCacheOrphan(ctx, &c, hoursZeroConnections)
			if err != nil {
				a.Logger.LogError("Error checking ElastiCache cluster", err, map[string]any{
					"cluster": aws.ToString(c.CacheClusterId),
				})
				return nil, nil
			}
			return orphan, nil
		},
		ToRow: func(r orphanElastiCacheCluster) []any {
			return []any{
				r.ClusterID,
				r.Engine,
				r.CacheNodeType,
				r.NumNodes,
				r.Status,
				r.CreatedDate,
				fmt.Sprintf("%d days", r.DaysSinceActivity),
				r.Reason,
			}
		},
		Delete: func(ctx context.Context, r orphanElastiCacheCluster) error {
			a.Logger.LogInfo("Deleting ElastiCache cluster", map[string]any{"ClusterId": r.ClusterID})
			if err := a.deleteElastiCacheCluster(ctx, r.ClusterID); err != nil {
				a.Logger.LogError("Failed to delete ElastiCache cluster", err, map[string]any{"ClusterId": r.ClusterID})
				return err
			}
			return nil
		},
		MonthlyCost: func(r orphanElastiCacheCluster) cost.USD {
			return cost.USD(r.NumNodes) * a.Pricing.ElastiCacheNodeMonth(r.CacheNodeType)
		},
	})
}

func (a *AWSCommand) checkElastiCacheOrphan(ctx context.Context, cluster *elasticachetypes.CacheCluster, hoursZeroConnections int64) (*orphanElastiCacheCluster, error) {
	clusterID := aws.ToString(cluster.CacheClusterId)
	window := time.Duration(hoursZeroConnections) * time.Hour
	dim := []cloudwatchtypes.Dimension{{Name: aws.String("CacheClusterId"), Value: aws.String(clusterID)}}

	idleConn, err := a.IsIdle(ctx, IdleSpec{
		Namespace:  "AWS/ElastiCache",
		MetricName: "CurrConnections",
		Dimensions: dim,
		Window:     window,
		Statistic:  cloudwatchtypes.StatisticMaximum,
	})
	if err != nil {
		return nil, err
	}
	hasConnections := !idleConn
	idleNetwork, err := a.IsIdle(ctx, IdleSpec{
		Namespace:  "AWS/ElastiCache",
		MetricName: "NetworkBytesIn",
		Dimensions: dim,
		Window:     window,
		Threshold:  1000,
	})
	if err != nil {
		return nil, err
	}
	hasNetworkActivity := !idleNetwork

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
