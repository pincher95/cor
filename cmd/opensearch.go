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
	"github.com/pincher95/cor/pkg/cost"
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

func (a *AWSCommand) executeOpenSearch(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	daysNoIndexing := int64((*extras)["days-no-indexing"].(int))
	hoursNoSearches := int64((*extras)["hours-no-searches"].(int))

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[string, orphanOpenSearchDomain]{
		Headers:       []string{"Domain Name", "Version", "Instance Type", "Instances", "Storage", "Created", "Days Since Activity", "Reason"},
		ResourceLabel: "OpenSearch domains",
		HideIndex:     true,
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
		// DescribeDomain is inlined here (not called from checkOpenSearchOrphan)
		// because the helper takes *DomainStatus rather than a domain-name string
		// like the other CloudWatch-enrichment helpers.
		Process: func(ctx context.Context, domainName string) (*orphanOpenSearchDomain, error) {
			domainOutput, err := a.AWSClient.OpenSearch.DescribeDomain(ctx, &opensearch.DescribeDomainInput{
				DomainName: aws.String(domainName),
			})
			if err != nil {
				a.Logger.LogError("Error describing OpenSearch domain", err, map[string]any{
					"domain": domainName,
				})
				return nil, nil
			}
			orphan, err := a.checkOpenSearchOrphan(ctx, domainOutput.DomainStatus, daysNoIndexing, hoursNoSearches)
			if err != nil {
				a.Logger.LogError("Error checking OpenSearch domain", err, map[string]any{
					"domain": domainName,
				})
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
				a.Logger.LogError("Failed to delete OpenSearch domain", err, map[string]any{"DomainName": r.DomainName})
				return err
			}
			return nil
		},
		MonthlyCost: func(r orphanOpenSearchDomain) cost.USD {
			nodes := cost.USD(r.InstanceCount) * a.Pricing.OpenSearchNodeMonth(r.InstanceType)
			// 0.135/GB-month is an industry-standard EBS rate for OpenSearch storage;
			// captured under EBSVolume["gp3"] which is close enough.
			storage := cost.USD(float64(r.StorageSize)) * a.Pricing.EBSVolumeGB("gp3")
			return nodes + storage
		},
	})
}

func (a *AWSCommand) checkOpenSearchOrphan(ctx context.Context, domain *opensearchtypes.DomainStatus, daysNoIndexing, hoursNoSearches int64) (*orphanOpenSearchDomain, error) {
	domainName := aws.ToString(domain.DomainName)
	dim := []cloudwatchtypes.Dimension{
		{Name: aws.String("DomainName"), Value: aws.String(domainName)},
		{Name: aws.String("ClientId"), Value: domain.ARN},
	}

	idleIndex, err := a.IsIdle(ctx, IdleSpec{
		Namespace:  "AWS/ES",
		MetricName: "IndexingRate",
		Dimensions: dim,
		Window:     time.Duration(daysNoIndexing) * 24 * time.Hour,
	})
	if err != nil {
		return nil, err
	}
	hasIndexing := !idleIndex

	idleSearch, err := a.IsIdle(ctx, IdleSpec{
		Namespace:  "AWS/ES",
		MetricName: "SearchRate",
		Dimensions: dim,
		Window:     time.Duration(hoursNoSearches) * time.Hour,
	})
	if err != nil {
		return nil, err
	}
	hasSearches := !idleSearch

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
