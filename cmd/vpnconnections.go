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
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
)

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

func (v *AWSCommand) executeVPNConnections(ctx context.Context, flagValues *map[string]any) error {
	rootCtx := ctx
	collectDeletes := (*flagValues)["delete"].(bool)
	filterByID := normalizeFilterValue((*flagValues)["filter-by-id"].(string))
	includeUp := (*flagValues)["include-up"].(bool)

	stream := printer.NewStreamTable(v.Output, true, []string{"VPN ID", "State", "Gateway", "TunnelsUp"})
	stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
	defer stream.Close()

	deleteIDs := make([]string, 0)
	page, err := v.AWSClient.EC2.DescribeVpnConnections(ctx, &ec2.DescribeVpnConnectionsInput{})
	if err != nil {
		return err
	}
	for _, vpn := range page.VpnConnections {
		vpnID := aws.ToString(vpn.VpnConnectionId)
		if filterByID != "" && !strings.Contains(vpnID, filterByID) {
			continue
		}
		tunnelsUp := countVPNTunnelsUp(vpn)
		if !includeUp && tunnelsUp > 0 {
			continue
		}
		state := string(vpn.State)
		gateway := "-"
		if vpn.VpnGatewayId != nil {
			gateway = aws.ToString(vpn.VpnGatewayId)
		} else if vpn.TransitGatewayId != nil {
			gateway = aws.ToString(vpn.TransitGatewayId)
		}

		stream.WriteRow(vpnID, state, gateway, tunnelsUp)
		if collectDeletes && tunnelsUp == 0 {
			deleteIDs = append(deleteIDs, vpnID)
		}
	}

	if collectDeletes {
		if len(deleteIDs) == 0 {
			return nil
		}
		confirm, err := confirmDelete(v.Prompter, v.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, id := range deleteIDs {
			v.Logger.LogInfo("Deleting VPN connection", map[string]any{"VpnConnectionId": id})
			if _, err := v.AWSClient.EC2.DeleteVpnConnection(rootCtx, &ec2.DeleteVpnConnectionInput{
				VpnConnectionId: aws.String(id),
			}); err != nil {
				return err
			}
		}
	}

	return nil
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
