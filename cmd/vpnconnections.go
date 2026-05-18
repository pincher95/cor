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

type orphanVPNConnection struct {
	id        string
	state     string
	gateway   string
	tunnelsUp int
}

var vpnConnectionsCmd = &cobra.Command{
	Use:   "vpnconnections",
	Short: "List and optionally delete Site-to-Site VPN connections with no tunnels up",
	Long:  `List Site-to-Site VPN connections where all tunnels are down and optionally delete them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-id", Type: "string"},
				{Name: "include-up", Type: "bool"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeVPNConnections)
	},
}

func init() {
	vpnConnectionsCmd.Flags().String("filter-by-id", "", "Filter VPN connections by ID (substring match).")
	vpnConnectionsCmd.Flags().Bool("include-up", false, "Include VPN connections with tunnels up.")
}

func (a *AWSCommand) executeVPNConnections(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByID := normalizeFilterValue((*extras)["filter-by-id"].(string))
	includeUp := (*extras)["include-up"].(bool)

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[ec2types.VpnConnection, orphanVPNConnection]{
		Headers:       []string{"VPN ID", "State", "Gateway", "TunnelsUp"},
		ResourceLabel: "Site-to-Site VPN connections",
		List: func(ctx context.Context, emit func(ec2types.VpnConnection) error) error {
			page, err := a.AWSClient.EC2.DescribeVpnConnections(ctx, &ec2.DescribeVpnConnectionsInput{})
			if err != nil {
				return err
			}
			for _, vpn := range page.VpnConnections {
				if err := emit(vpn); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, vpn ec2types.VpnConnection) (*orphanVPNConnection, error) {
			vpnID := aws.ToString(vpn.VpnConnectionId)
			if !matchesFilterValue(vpnID, filterByID) {
				return nil, nil
			}
			tunnelsUp := countVPNTunnelsUp(vpn)
			if !includeUp && tunnelsUp > 0 {
				return nil, nil
			}
			gateway := "-"
			if vpn.VpnGatewayId != nil {
				gateway = aws.ToString(vpn.VpnGatewayId)
			} else if vpn.TransitGatewayId != nil {
				gateway = aws.ToString(vpn.TransitGatewayId)
			}
			return &orphanVPNConnection{
				id:        vpnID,
				state:     string(vpn.State),
				gateway:   gateway,
				tunnelsUp: tunnelsUp,
			}, nil
		},
		ToRow: func(r orphanVPNConnection) []any {
			return []any{r.id, r.state, r.gateway, r.tunnelsUp}
		},
		Delete: func(ctx context.Context, r orphanVPNConnection) error {
			if r.tunnelsUp > 0 {
				return nil
			}
			a.Logger.LogInfo("Deleting VPN connection", map[string]any{"VpnConnectionId": r.id})
			_, err := a.AWSClient.EC2.DeleteVpnConnection(ctx, &ec2.DeleteVpnConnectionInput{
				VpnConnectionId: aws.String(r.id),
			})
			return err
		},
		MonthlyCost: func(r orphanVPNConnection) cost.USD {
			return a.Pricing.SiteToSiteVPNMonth()
		},
	})
}

func countVPNTunnelsUp(vpn ec2types.VpnConnection) int {
	count := 0
	for _, t := range vpn.VgwTelemetry {
		if t.Status == ec2types.TelemetryStatusUp {
			count++
		}
	}
	return count
}
