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
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
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
				return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeNatGateways)
	},
}

func init() {
	natgatewaysCmd.Flags().String("filter-by-state", "available", "Filter NAT Gateways by state (available, deleted, deleting, failed, pending)")
}

type orphanNatGateway struct {
	name    string
	id      string
	state   string
	vpcID   string
	subnet  string
	created string
}

func (c *AWSCommand) executeNatGateways(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	stateFilter := (*extras)["filter-by-state"].(string)

	return runOrphanPipeline(c, ctx, globals, extras, OrphanPipeline[types.NatGateway, orphanNatGateway]{
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
			p := ec2.NewDescribeNatGatewaysPaginator(c.AWSClient.EC2, &ec2.DescribeNatGatewaysInput{Filter: filters})
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
		Process: func(_ context.Context, ng types.NatGateway) (*orphanNatGateway, error) {
			name := "-"
			for _, tag := range ng.Tags {
				if tag.Key != nil && *tag.Key == "Name" && tag.Value != nil {
					name = *tag.Value
					break
				}
			}
			return &orphanNatGateway{
				name:    name,
				id:      aws.ToString(ng.NatGatewayId),
				state:   string(ng.State),
				vpcID:   aws.ToString(ng.VpcId),
				subnet:  aws.ToString(ng.SubnetId),
				created: ng.CreateTime.String(),
			}, nil
		},
		ToRow: func(r orphanNatGateway) []any {
			return []any{r.name, r.id, r.state, r.vpcID, r.subnet, r.created}
		},
		Delete: func(ctx context.Context, r orphanNatGateway) error {
			c.Logger.LogInfo("Deleting NAT Gateway", map[string]any{"ID": r.id, "Name": r.name})
			_, err := c.AWSClient.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{NatGatewayId: aws.String(r.id)})
			return err
		},
	})
}
