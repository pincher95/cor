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
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/utils"
	"github.com/spf13/cobra"
)

type orphanVPCEndpoint struct {
	name     string
	id       string
	service  string
	epType   ec2types.VpcEndpointType
	state    string
	eniCount int
	subnets  int
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
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeVPCEndpoints)
	},
}

func init() {
	vpcEndpointsCmd.Flags().String("filter-by-service", "", "Filter by VPC endpoint service name (substring match).")
	vpcEndpointsCmd.Flags().Bool("include-attached", false, "Include endpoints that have network interfaces attached.")
	vpcEndpointsCmd.Flags().Bool("include-non-interface", false, "Include non-interface endpoints (gateway endpoints are typically free).")
}

func (a *AWSCommand) executeVPCEndpoints(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByService := normalizeFilterValue((*extras)["filter-by-service"].(string))
	includeAttached := (*extras)["include-attached"].(bool)
	includeNonInterface := (*extras)["include-non-interface"].(bool)

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[ec2types.VpcEndpoint, orphanVPCEndpoint]{
		Headers:       []string{"Name", "Endpoint ID", "Service", "Type", "State", "ENIs"},
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
		Process: func(_ context.Context, ep ec2types.VpcEndpoint) (*orphanVPCEndpoint, error) {
			service := aws.ToString(ep.ServiceName)
			if !matchesFilterValue(service, filterByService) {
				return nil, nil
			}
			eniCount := len(ep.NetworkInterfaceIds)
			if !includeAttached && eniCount > 0 {
				return nil, nil
			}
			name := "-"
			if tag, ok := utils.TagsToMap(ep.Tags)["Name"]; ok && tag.Value != nil {
				name = *tag.Value
			}
			return &orphanVPCEndpoint{
				name:     name,
				id:       aws.ToString(ep.VpcEndpointId),
				service:  service,
				epType:   ep.VpcEndpointType,
				state:    string(ep.State),
				eniCount: eniCount,
				subnets:  len(ep.SubnetIds),
			}, nil
		},
		ToRow: func(r orphanVPCEndpoint) []any {
			return []any{r.name, r.id, r.service, string(r.epType), r.state, r.eniCount}
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
