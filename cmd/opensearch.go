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
	"github.com/aws/aws-sdk-go-v2/service/opensearch"
	opensearchtypes "github.com/aws/aws-sdk-go-v2/service/opensearch/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanOpenSearchDomain struct {
	DomainName        string
	EngineVersion     string
	InstanceType      string
	InstanceCount     int
	StorageSize       int32
	Created           string
	NoIndexing        bool
	NoSearching       bool
	DaysSinceActivity int64
	Reason            string
}

// opensearchCmd represents the opensearch command
var opensearchCmd = &cobra.Command{
	Use:   "opensearch",
	Short: "List orphaned OpenSearch domains",
	Long: `Finds OpenSearch/Elasticsearch domains that are potentially orphaned based on:
- No indexing operations in the last N days (default: 7)
- Zero search requests in the last 24 hours
- Combination of both indicating no active usage

Orphaned OpenSearch domains can incur significant costs:
- t3.small.search: ~$26/month
- r6g.large.search: ~$101/month
- Storage: $0.135/GB-month (EBS)
- Data transfer costs`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "days-no-indexing", Type: "int"},
				{Name: "hours-no-searches", Type: "int"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{
					OpenSearch: opensearch.NewFromConfig(*cfg),
					CloudWatch: cloudwatch.NewFromConfig(*cfg),
				}
			},
		}, (*AWSCommand).executeOpenSearch)
	},
}

func init() {
	opensearchCmd.Flags().Int("days-no-indexing", 7, "Consider domains orphaned if no indexing for this many days")
	opensearchCmd.Flags().Int("hours-no-searches", 24, "Consider domains orphaned if no searches for this many hours")
}

func (a *AWSCommand) executeOpenSearch(ctx context.Context, flagValues *map[string]any) error {
	daysNoIndexing := int64((*flagValues)["days-no-indexing"].(int))
	hoursNoSearches := int64((*flagValues)["hours-no-searches"].(int))

	return runOrphanPipeline(a, ctx, flagValues, OrphanPipeline[string, orphanOpenSearchDomain]{
		Headers:   []string{"Domain Name", "Version", "Instance Type", "Instances", "Storage", "Created", "Days Since Activity", "Reason"},
		HideIndex: true,
		List: func(ctx context.Context, emit func(string) error) error {
			out, err := a.AWSClient.OpenSearch.ListDomainNames(ctx, &opensearch.ListDomainNamesInput{})
			if err != nil {
				return err
			}
			for _, d := range out.DomainNames {
				if err := emit(aws.ToString(d.DomainName)); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(ctx context.Context, domainName string) (*orphanOpenSearchDomain, error) {
			domainOutput, err := a.AWSClient.OpenSearch.DescribeDomain(ctx, &opensearch.DescribeDomainInput{
				DomainName: aws.String(domainName),
			})
			if err != nil {
				a.Logger.LogError("Error describing OpenSearch domain", err, map[string]any{
					"domain": domainName,
				}, false)
				return nil, nil
			}
			orphan, err := a.checkOpenSearchOrphan(ctx, domainOutput.DomainStatus, daysNoIndexing, hoursNoSearches)
			if err != nil {
				a.Logger.LogError("Error checking OpenSearch domain", err, map[string]any{
					"domain": domainName,
				}, false)
				return nil, nil
			}
			return orphan, nil
		},
		ToRow: func(r orphanOpenSearchDomain) []any {
			return []any{
				r.DomainName,
				r.EngineVersion,
				r.InstanceType,
				r.InstanceCount,
				fmt.Sprintf("%d GB", r.StorageSize),
				r.Created,
				fmt.Sprintf("%d days", r.DaysSinceActivity),
				r.Reason,
			}
		},
		Delete: func(ctx context.Context, r orphanOpenSearchDomain) error {
			a.Logger.LogInfo("Deleting OpenSearch domain", map[string]any{"DomainName": r.DomainName})
			if err := a.deleteOpenSearchDomain(ctx, r.DomainName); err != nil {
				a.Logger.LogError("Failed to delete OpenSearch domain", err, map[string]any{"DomainName": r.DomainName}, false)
				return err
			}
			return nil
		},
	})
}

func (a *AWSCommand) checkOpenSearchOrphan(ctx context.Context, domain *opensearchtypes.DomainStatus, daysNoIndexing, hoursNoSearches int64) (*orphanOpenSearchDomain, error) {
	domainName := aws.ToString(domain.DomainName)

	// Check indexing operations metric
	endTime := time.Now()
	indexingStartTime := endTime.Add(-time.Duration(daysNoIndexing) * 24 * time.Hour)

	indexingInput := &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/ES"), // Both ES and OpenSearch use AWS/ES namespace
		MetricName: aws.String("IndexingRate"),
		Dimensions: []cloudwatchtypes.Dimension{
			{
				Name:  aws.String("DomainName"),
				Value: aws.String(domainName),
			},
			{
				Name:  aws.String("ClientId"),
				Value: domain.ARN, // Use account ID from ARN
			},
		},
		StartTime:  &indexingStartTime,
		EndTime:    &endTime,
		Period:     aws.Int32(86400), // 1 day
		Statistics: []cloudwatchtypes.Statistic{cloudwatchtypes.StatisticSum},
	}

	indexingOutput, err := a.AWSClient.CloudWatch.GetMetricStatistics(ctx, indexingInput)
	if err != nil {
		return nil, err
	}

	hasIndexing := false
	for _, datapoint := range indexingOutput.Datapoints {
		if datapoint.Sum != nil && *datapoint.Sum > 0 {
			hasIndexing = true
			break
		}
	}

	// Check search requests metric
	searchStartTime := endTime.Add(-time.Duration(hoursNoSearches) * time.Hour)

	searchInput := &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/ES"),
		MetricName: aws.String("SearchRate"),
		Dimensions: []cloudwatchtypes.Dimension{
			{
				Name:  aws.String("DomainName"),
				Value: aws.String(domainName),
			},
			{
				Name:  aws.String("ClientId"),
				Value: domain.ARN,
			},
		},
		StartTime:  &searchStartTime,
		EndTime:    &endTime,
		Period:     aws.Int32(3600), // 1 hour
		Statistics: []cloudwatchtypes.Statistic{cloudwatchtypes.StatisticSum},
	}

	searchOutput, err := a.AWSClient.CloudWatch.GetMetricStatistics(ctx, searchInput)
	if err != nil {
		return nil, err
	}

	hasSearches := false
	for _, datapoint := range searchOutput.Datapoints {
		if datapoint.Sum != nil && *datapoint.Sum > 0 {
			hasSearches = true
			break
		}
	}

	// Determine if orphaned (must have both no indexing AND no searches)
	if hasIndexing || hasSearches {
		return nil, nil
	}

	// OpenSearch DomainStatus doesn't expose creation date directly
	// We'll estimate based on search/indexing activity period
	daysSinceCreation := daysNoIndexing
	createdDate := "N/A"

	instanceCount := 1
	instanceType := "unknown"
	if domain.ClusterConfig != nil {
		if domain.ClusterConfig.InstanceCount != nil {
			instanceCount = int(*domain.ClusterConfig.InstanceCount)
		}
		if domain.ClusterConfig.InstanceType != "" {
			instanceType = string(domain.ClusterConfig.InstanceType)
		}
	}

	storageSize := int32(0)
	if domain.EBSOptions != nil && domain.EBSOptions.VolumeSize != nil {
		storageSize = *domain.EBSOptions.VolumeSize * int32(instanceCount)
	}

	return &orphanOpenSearchDomain{
		DomainName:        domainName,
		EngineVersion:     aws.ToString(domain.EngineVersion),
		InstanceType:      instanceType,
		InstanceCount:     instanceCount,
		StorageSize:       storageSize,
		Created:           createdDate,
		NoIndexing:        !hasIndexing,
		NoSearching:       !hasSearches,
		DaysSinceActivity: daysSinceCreation,
		Reason:            fmt.Sprintf("No indexing (%dd) & no searches (%dh)", daysNoIndexing, hoursNoSearches),
	}, nil
}

func (a *AWSCommand) deleteOpenSearchDomain(ctx context.Context, domainName string) error {
	_, err := a.AWSClient.OpenSearch.DeleteDomain(ctx, &opensearch.DeleteDomainInput{
		DomainName: aws.String(domainName),
	})
	return err
}
