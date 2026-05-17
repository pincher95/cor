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
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

// orphanECSCluster predates the orphanX + lowercase-fields naming convention
// used by orphanVolume / orphanNatGateway / orphanTargetGroup. Left as-is
// because checkECSOrphan (also pre-existing) references the exported fields.
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

func (a *AWSCommand) executeECS(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[string, orphanECSCluster]{
		Headers:       []string{"Cluster Name", "Status", "Registered Tasks", "Running Tasks", "Services", "Reason"},
		ResourceLabel: "ECS clusters",
		HideIndex:     true,
		List: func(ctx context.Context, emit func(string) error) error {
			p := ecs.NewListClustersPaginator(a.AWSClient.ECS, &ecs.ListClustersInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, clusterARN := range page.ClusterArns {
					if err := emit(clusterARN); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, clusterARN string) (*orphanECSCluster, error) {
			orphan, err := a.checkECSOrphan(ctx, clusterARN)
			if err != nil {
				a.Logger.LogError("Error checking ECS cluster", err, map[string]any{
					"cluster": clusterARN,
				})
				return nil, nil // skip this cluster, continue the pipeline
			}
			return orphan, nil
		},
		ToRow: func(r orphanECSCluster) []any {
			return []any{r.ClusterName, r.Status, r.RegisteredTasks, r.RunningTasks, r.ServicesCount, r.Reason}
		},
		Delete: func(ctx context.Context, r orphanECSCluster) error {
			a.Logger.LogInfo("Deleting ECS cluster", map[string]any{"cluster": r.ClusterName})
			return a.deleteECSCluster(ctx, r.ClusterARN)
		},
	})
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
