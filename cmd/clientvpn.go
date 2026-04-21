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

func (c *AWSCommand) executeClientVPN(ctx context.Context, flagValues *map[string]any) error {
	rootCtx := ctx
	collectDeletes := (*flagValues)["delete"].(bool)
	filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))
	includeActive := (*flagValues)["include-active"].(bool)

	stream := printer.NewStreamTable(c.Output, true, []string{"Endpoint ID", "Description", "Status", "ActiveConnections"})
	stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
	defer stream.Close()

	deleteIDs := make([]string, 0)
	paginator := ec2.NewDescribeClientVpnEndpointsPaginator(c.AWSClient.EC2, &ec2.DescribeClientVpnEndpointsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, ep := range page.ClientVpnEndpoints {
			desc := aws.ToString(ep.Description)
			if filterByName != "" && !strings.Contains(desc, filterByName) {
				continue
			}
			activeCount, err := c.countActiveClientVPNConnections(ctx, aws.ToString(ep.ClientVpnEndpointId))
			if err != nil {
				return err
			}
			if !includeActive && activeCount > 0 {
				continue
			}
			status := "-"
			if ep.Status != nil {
				status = string(ep.Status.Code)
			}
			stream.WriteRow(aws.ToString(ep.ClientVpnEndpointId), desc, status, activeCount)
			if collectDeletes && activeCount == 0 {
				deleteIDs = append(deleteIDs, aws.ToString(ep.ClientVpnEndpointId))
			}
		}
	}

	if collectDeletes {
		if len(deleteIDs) == 0 {
			return nil
		}
		confirm, err := confirmDelete(c.Prompter, c.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, id := range deleteIDs {
			c.Logger.LogInfo("Deleting Client VPN endpoint", map[string]any{"EndpointId": id})
			if _, err := c.AWSClient.EC2.DeleteClientVpnEndpoint(rootCtx, &ec2.DeleteClientVpnEndpointInput{
				ClientVpnEndpointId: aws.String(id),
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *AWSCommand) countActiveClientVPNConnections(ctx context.Context, endpointID string) (int, error) {
	if endpointID == "" {
		return 0, nil
	}
	filter := ec2types.Filter{
		Name:   aws.String("status.code"),
		Values: []string{"active"},
	}
	p := ec2.NewDescribeClientVpnConnectionsPaginator(c.AWSClient.EC2, &ec2.DescribeClientVpnConnectionsInput{
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
