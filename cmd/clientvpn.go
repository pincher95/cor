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
	"github.com/spf13/cobra"
)

type orphanClientVPN struct {
	id                string
	description       string
	status            string
	activeConns       int
	associatedSubnets int
}

var clientVPNCmd = &cobra.Command{
	Use:   "clientvpn",
	Short: "List and optionally delete Client VPN endpoints with zero active connections",
	Long:  `List Client VPN endpoints that have no active connections and optionally delete them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "include-active", Type: "bool"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeClientVPN)
	},
}

func init() {
	clientVPNCmd.Flags().String("filter-by-name", "", "Filter Client VPN endpoints by description (substring match).")
	clientVPNCmd.Flags().Bool("include-active", false, "Include endpoints with active connections.")
}

func (a *AWSCommand) executeClientVPN(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByName := normalizeFilterValue((*extras)["filter-by-name"].(string))
	includeActive := (*extras)["include-active"].(bool)

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[ec2types.ClientVpnEndpoint, orphanClientVPN]{
		Headers:       []string{"Endpoint ID", "Description", "Status", "ActiveConnections", "AssocSubnets"},
		ResourceLabel: "Client VPN endpoints",
		List: func(ctx context.Context, emit func(ec2types.ClientVpnEndpoint) error) error {
			p := ec2.NewDescribeClientVpnEndpointsPaginator(a.AWSClient.EC2, &ec2.DescribeClientVpnEndpointsInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, ep := range page.ClientVpnEndpoints {
					if err := emit(ep); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, ep ec2types.ClientVpnEndpoint) (*orphanClientVPN, error) {
			desc := aws.ToString(ep.Description)
			if !matchesFilterValue(desc, filterByName) {
				return nil, nil
			}
			id := aws.ToString(ep.ClientVpnEndpointId)
			activeCount, err := a.countActiveClientVPNConnections(ctx, id)
			if err != nil {
				return nil, err
			}
			if !includeActive && activeCount > 0 {
				return nil, nil
			}
			subnetCount, err := a.countClientVPNAssociatedSubnets(ctx, id)
			if err != nil {
				return nil, err
			}
			status := "-"
			if ep.Status != nil {
				status = string(ep.Status.Code)
			}
			return &orphanClientVPN{
				id:                id,
				description:       desc,
				status:            status,
				activeConns:       activeCount,
				associatedSubnets: subnetCount,
			}, nil
		},
		ToRow: func(r orphanClientVPN) []any {
			return []any{r.id, r.description, r.status, r.activeConns, r.associatedSubnets}
		},
		Delete: func(ctx context.Context, r orphanClientVPN) error {
			if r.activeConns > 0 {
				return nil
			}
			a.Logger.LogInfo("Deleting Client VPN endpoint", map[string]any{"EndpointId": r.id})
			_, err := a.AWSClient.EC2.DeleteClientVpnEndpoint(ctx, &ec2.DeleteClientVpnEndpointInput{
				ClientVpnEndpointId: aws.String(r.id),
			})
			return err
		},
		MonthlyCost: func(r orphanClientVPN) cost.USD {
			// Billed per associated subnet, not per endpoint.
			return cost.USD(r.associatedSubnets) * a.Pricing.ClientVPNEndpointMonth()
		},
	})
}

// countClientVPNAssociatedSubnets returns the number of subnets associated
// with the endpoint. Client VPN bills per associated subnet, so an endpoint
// with zero associations is genuinely free.
func (a *AWSCommand) countClientVPNAssociatedSubnets(ctx context.Context, endpointID string) (int, error) {
	if endpointID == "" {
		return 0, nil
	}
	p := ec2.NewDescribeClientVpnTargetNetworksPaginator(a.AWSClient.EC2, &ec2.DescribeClientVpnTargetNetworksInput{
		ClientVpnEndpointId: aws.String(endpointID),
		MaxResults:          aws.Int32(20),
	})
	count := 0
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return 0, err
		}
		for _, n := range page.ClientVpnTargetNetworks {
			// Only "associated" status counts as billable.
			if n.Status != nil && n.Status.Code == ec2types.AssociationStatusCodeAssociated {
				count++
			}
		}
	}
	return count, nil
}

func (a *AWSCommand) countActiveClientVPNConnections(ctx context.Context, endpointID string) (int, error) {
	if endpointID == "" {
		return 0, nil
	}
	filter := ec2types.Filter{
		Name:   aws.String("status.code"),
		Values: []string{"active"},
	}
	p := ec2.NewDescribeClientVpnConnectionsPaginator(a.AWSClient.EC2, &ec2.DescribeClientVpnConnectionsInput{
		ClientVpnEndpointId: aws.String(endpointID),
		Filters:             []ec2types.Filter{filter},
		MaxResults:          aws.Int32(50),
	})
	count := 0
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return 0, err
		}
		count += len(page.Connections)
		if count > 0 {
			break
		}
	}
	return count, nil
}
