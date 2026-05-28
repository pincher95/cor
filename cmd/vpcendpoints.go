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
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/utils"
	"github.com/spf13/cobra"
)

type orphanVPCEndpoint struct {
	name              string
	id                string
	service           string
	epType            ec2types.VpcEndpointType
	state             string
	eniCount          int
	subnets           int
	newConnections    int64
	privateDNSEnabled bool
	reason            string
}

var vpcEndpointsCmd = &cobra.Command{
	Use:   "vpcendpoints",
	Short: "List and optionally delete orphan interface VPC endpoints",
	Long:  `List interface VPC endpoints that have no network interfaces and optionally delete them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-service", Type: "string"},
				{Name: "include-attached", Type: "bool"},
				{Name: "include-non-interface", Type: "bool"},
				{Name: "max-idle-days", Type: "int"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{
					EC2:        ec2.NewFromConfig(*cfg),
					CloudWatch: cloudwatch.NewFromConfig(*cfg),
				}
			},
		}, (*AWSCommand).executeVPCEndpoints)
	},
}

func init() {
	vpcEndpointsCmd.Flags().String("filter-by-service", "", "Filter by VPC endpoint service name (substring match).")
	vpcEndpointsCmd.Flags().Bool("include-attached", false, "Include endpoints that have network interfaces attached, even if traffic is non-zero.")
	vpcEndpointsCmd.Flags().Bool("include-non-interface", false, "Include non-interface endpoints (gateway endpoints are typically free).")
	vpcEndpointsCmd.Flags().Int("max-idle-days", 30, "Flag interface endpoints with zero BytesProcessed over this many days as idle. 0 disables the traffic check.")
}

func (a *AWSCommand) executeVPCEndpoints(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByService := normalizeFilterValue((*extras)["filter-by-service"].(string))
	includeAttached := (*extras)["include-attached"].(bool)
	includeNonInterface := (*extras)["include-non-interface"].(bool)
	maxIdleDays := 30
	if v, ok := (*extras)["max-idle-days"].(int); ok && v >= 0 {
		maxIdleDays = v
	}

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[ec2types.VpcEndpoint, orphanVPCEndpoint]{
		Headers:       []string{"Name", "Endpoint ID", "Service", "Type", "State", "ENIs", "PrivDNS", fmt.Sprintf("Conns (%dd)", maxIdleDays), "Reason"},
		ResourceLabel: "VPC endpoints",
		List: func(ctx context.Context, emit func(ec2types.VpcEndpoint) error) error {
			filters := []ec2types.Filter{}
			if !includeNonInterface {
				filters = append(filters, ec2types.Filter{
					Name:   aws.String("vpc-endpoint-type"),
					Values: []string{string(ec2types.VpcEndpointTypeInterface)},
				})
			}
			p := ec2.NewDescribeVpcEndpointsPaginator(a.AWSClient.EC2, &ec2.DescribeVpcEndpointsInput{
				Filters: filters,
			})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, ep := range page.VpcEndpoints {
					if err := emit(ep); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, ep ec2types.VpcEndpoint) (*orphanVPCEndpoint, error) {
			service := aws.ToString(ep.ServiceName)
			if !matchesFilterValue(service, filterByService) {
				return nil, nil
			}
			// Transient states aren't orphans — they're either being created or removed.
			// AWS returns "available" (lowercase); ec2types.StateAvailable is "Available".
			if !includeAttached && !strings.EqualFold(string(ep.State), string(ec2types.StateAvailable)) {
				return nil, nil
			}
			eniCount := len(ep.NetworkInterfaceIds)
			id := aws.ToString(ep.VpcEndpointId)
			isInterface := ep.VpcEndpointType == ec2types.VpcEndpointTypeInterface
			privateDNS := aws.ToBool(ep.PrivateDnsEnabled)

			// Endpoints created within the lookback window haven't had time
			// to accumulate metrics; a 0-connection reading is meaningless.
			// PrivateDnsEnabled=false is intentionally NOT a reason: shared-
			// services VPCs (centralized DNS via Route 53 Profiles / self-
			// managed PHZs) and custom PrivateLink to third-party services
			// both legitimately set it to false. Shown as a diagnostic column.
			matureEnough := ep.CreationTimestamp != nil &&
				time.Since(*ep.CreationTimestamp) >= time.Duration(maxIdleDays)*24*time.Hour

			var conns int64
			if isInterface && maxIdleDays > 0 && matureEnough {
				conns, _ = a.vpcEndpointNewConnections(ctx, ep, maxIdleDays)
			}

			reason := ""
			switch {
			case eniCount == 0:
				reason = "No network interfaces"
			case isInterface && maxIdleDays > 0 && matureEnough && conns == 0:
				reason = fmt.Sprintf("No connections in %d days", maxIdleDays)
			}
			if reason == "" && !includeAttached {
				return nil, nil
			}

			name := "-"
			if tag, ok := utils.TagsToMap(ep.Tags)["Name"]; ok && tag.Value != nil {
				name = *tag.Value
			}
			return &orphanVPCEndpoint{
				name:              name,
				id:                id,
				service:           service,
				epType:            ep.VpcEndpointType,
				state:             string(ep.State),
				eniCount:          eniCount,
				subnets:           len(ep.SubnetIds),
				newConnections:    conns,
				privateDNSEnabled: privateDNS,
				reason:            reason,
			}, nil
		},
		ToRow: func(r orphanVPCEndpoint) []any {
			connsCell := any("—")
			dnsCell := any("—")
			if r.epType == ec2types.VpcEndpointTypeInterface {
				connsCell = r.newConnections
				if r.privateDNSEnabled {
					dnsCell = "Yes"
				} else {
					dnsCell = "No"
				}
			}
			return []any{r.name, r.id, r.service, string(r.epType), r.state, r.eniCount, dnsCell, connsCell, r.reason}
		},
		MonthlyCost: func(r orphanVPCEndpoint) cost.USD {
			if r.epType != ec2types.VpcEndpointTypeInterface {
				return 0
			}
			n := r.subnets
			if n == 0 {
				n = 1
			}
			return cost.USD(n) * a.Pricing.InterfaceEndpointMonth()
		},
		DeleteBatch: func(ctx context.Context, rs []orphanVPCEndpoint) error {
			ids := make([]string, 0, len(rs))
			for _, r := range rs {
				if r.eniCount > 0 {
					continue
				}
				ids = append(ids, r.id)
			}
			for _, chunk := range utils.SliceChunkBy(ids, 25) {
				if len(chunk) == 0 {
					continue
				}
				a.Logger.LogInfo("Deleting VPC endpoints", map[string]any{"count": len(chunk)})
				if _, err := a.AWSClient.EC2.DeleteVpcEndpoints(ctx, &ec2.DeleteVpcEndpointsInput{
					VpcEndpointIds: chunk,
				}); err != nil {
					return err
				}
			}
			return nil
		},
	})
}

// vpcEndpointNewConnections returns the sum of NewConnections over the
// last `days`. NewConnections is published at the endpoint level (4
// dimensions; no Subnet Id), so one query gives the total. Endpoints
// where the metric doesn't exist (very new, or not yet emitting) return
// 0 with no error.
func (a *AWSCommand) vpcEndpointNewConnections(ctx context.Context, ep ec2types.VpcEndpoint, days int) (int64, error) {
	if a.AWSClient.CloudWatch == nil || days <= 0 {
		return 0, nil
	}
	id := aws.ToString(ep.VpcEndpointId)
	vpcID := aws.ToString(ep.VpcId)
	service := aws.ToString(ep.ServiceName)
	if id == "" || vpcID == "" || service == "" {
		return 0, nil
	}
	end := time.Now()
	start := end.Add(-time.Duration(days) * 24 * time.Hour)
	out, err := a.AWSClient.CloudWatch.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/PrivateLinkEndpoints"),
		MetricName: aws.String("NewConnections"),
		Dimensions: []cwtypes.Dimension{
			{Name: aws.String("VPC Endpoint Id"), Value: aws.String(id)},
			{Name: aws.String("VPC Id"), Value: aws.String(vpcID)},
			{Name: aws.String("Endpoint Type"), Value: aws.String(string(ec2types.VpcEndpointTypeInterface))},
			{Name: aws.String("Service Name"), Value: aws.String(service)},
		},
		StartTime:  &start,
		EndTime:    &end,
		Period:     aws.Int32(86400),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticSum},
	})
	if err != nil {
		return 0, err
	}
	var total int64
	for _, dp := range out.Datapoints {
		if dp.Sum != nil {
			total += int64(*dp.Sum)
		}
	}
	return total, nil
}
