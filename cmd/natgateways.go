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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

// natgatewaysCmd represents the natgateways command
var natgatewaysCmd = &cobra.Command{
	Use:   "natgateways",
	Short: "List and optionally delete NAT Gateways",
	Long:  `Find NAT Gateways that might be unused. NAT Gateways are billed hourly and for data processing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-state", Type: "string"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{
					EC2:        ec2.NewFromConfig(*cfg),
					CloudWatch: cloudwatch.NewFromConfig(*cfg),
				}
			},
		}, (*AWSCommand).executeNatGateways)
	},
}

func init() {
	natgatewaysCmd.Flags().String("filter-by-state", "available", "Filter NAT Gateways by state (available, deleted, deleting, failed, pending)")
}

type orphanNatGateway struct {
	name          string
	id            string
	state         string
	vpcID         string
	subnet        string
	created       string
	bytesOutMonth int64
}

func (a *AWSCommand) executeNatGateways(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	stateFilter := (*extras)["filter-by-state"].(string)

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[types.NatGateway, orphanNatGateway]{
		Headers:       []string{"Name", "ID", "State", "VPC", "Subnet", "Created"},
		ResourceLabel: "NAT Gateways",
		List: func(ctx context.Context, emit func(types.NatGateway) error) error {
			filters := []types.Filter{}
			if stateFilter != "" {
				filters = append(filters, types.Filter{
					Name:   aws.String("state"),
					Values: []string{stateFilter},
				})
			}
			p := ec2.NewDescribeNatGatewaysPaginator(a.AWSClient.EC2, &ec2.DescribeNatGatewaysInput{Filter: filters})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, ng := range page.NatGateways {
					if err := emit(ng); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, ng types.NatGateway) (*orphanNatGateway, error) {
			name := ec2NameTag(ng.Tags)
			if name == "" {
				name = "-"
			}
			natID := aws.ToString(ng.NatGatewayId)
			bytesOut, _ := a.natBytesOutLast30d(ctx, natID)
			return &orphanNatGateway{
				name:          name,
				id:            natID,
				state:         string(ng.State),
				vpcID:         aws.ToString(ng.VpcId),
				subnet:        aws.ToString(ng.SubnetId),
				created:       ng.CreateTime.String(),
				bytesOutMonth: bytesOut,
			}, nil
		},
		ToRow: func(r orphanNatGateway) []any {
			return []any{r.name, r.id, r.state, r.vpcID, r.subnet, r.created}
		},
		Delete: func(ctx context.Context, r orphanNatGateway) error {
			a.Logger.LogInfo("Deleting NAT Gateway", map[string]any{"ID": r.id, "Name": r.name})
			_, err := a.AWSClient.EC2.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{NatGatewayId: aws.String(r.id)})
			return err
		},
		MonthlyCost: func(r orphanNatGateway) cost.USD {
			idle := cost.USD(cost.HoursPerMonth) * a.Pricing.NATGatewayHour()
			dataGB := float64(r.bytesOutMonth) / (1024 * 1024 * 1024)
			return idle + cost.USD(dataGB)*a.Pricing.NATGatewayDataGB()
		},
	})
}

// natBytesOutLast30d returns total bytes processed (outbound + return) over
// the last 30 days from CloudWatch AWS/NATGateway metrics. Idle NATs return
// 0; an unreachable/missing-metric NAT also returns 0 with no error.
func (a *AWSCommand) natBytesOutLast30d(ctx context.Context, natID string) (int64, error) {
	if a.AWSClient.CloudWatch == nil || natID == "" {
		return 0, nil
	}
	end := time.Now()
	start := end.Add(-30 * 24 * time.Hour)
	dims := []cwtypes.Dimension{{Name: aws.String("NatGatewayId"), Value: aws.String(natID)}}

	var total int64
	for _, metric := range []string{"BytesOutToDestination", "BytesOutToSource"} {
		out, err := a.AWSClient.CloudWatch.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
			Namespace:  aws.String("AWS/NATGateway"),
			MetricName: aws.String(metric),
			Dimensions: dims,
			StartTime:  &start,
			EndTime:    &end,
			Period:     aws.Int32(86400),
			Statistics: []cwtypes.Statistic{cwtypes.StatisticSum},
		})
		if err != nil {
			return 0, err
		}
		for _, dp := range out.Datapoints {
			if dp.Sum != nil {
				total += int64(*dp.Sum)
			}
		}
	}
	return total, nil
}
