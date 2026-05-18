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
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/utils"
	"github.com/spf13/cobra"
)

type orphanElasticIP struct {
	name, allocationID, publicIP, associationID, networkInterfaceID string
}

// elasticaddressesCmd represents the elasticaddresses command
var elasticIPsCmd = &cobra.Command{
	Use:   "elasticips",
	Short: "List and optionally release unassociated Elastic IPs",
	Long:  `List Elastic IP addresses that are not associated to any instance/network interface and optionally release them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeElasticIPs)
	},
}

func init() {
	elasticIPsCmd.Flags().String("filter-by-name", "", "Filter Elastic IPs by tag:Name (empty = no filter).")
}

func (a *AWSCommand) executeElasticIPs(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByName := normalizeFilterValue((*extras)["filter-by-name"].(string))

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[types.Address, orphanElasticIP]{
		Headers:       []string{"Name", "Allocation ID", "Allocated Public address", "Association ID", "Network interface ID"},
		ResourceLabel: "Elastic IPs",
		List: func(ctx context.Context, emit func(types.Address) error) error {
			filters := []types.Filter{}
			if filterByName != "" {
				filters = append(filters, types.Filter{
					Name:   aws.String("tag:Name"),
					Values: []string{filterByName},
				})
			}
			out, err := a.AWSClient.EC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: filters})
			if err != nil {
				return err
			}
			for _, addr := range out.Addresses {
				if err := emit(addr); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, addr types.Address) (*orphanElasticIP, error) {
			// Orphan only if neither associated nor attached to an instance.
			if addr.AssociationId != nil || addr.InstanceId != nil {
				return nil, nil
			}
			name := "-"
			if nameTag, ok := utils.TagsToMap(addr.Tags)["Name"]; ok && nameTag.Value != nil {
				name = *nameTag.Value
			}
			return &orphanElasticIP{
				name:               name,
				allocationID:       stringOrDash(addr.AllocationId),
				publicIP:           stringOrDash(addr.PublicIp),
				associationID:      stringOrDash(addr.AssociationId),
				networkInterfaceID: stringOrDash(addr.NetworkInterfaceId),
			}, nil
		},
		ToRow: func(r orphanElasticIP) []any {
			return []any{r.name, r.allocationID, r.publicIP, r.associationID, r.networkInterfaceID}
		},
		Delete: func(ctx context.Context, r orphanElasticIP) error {
			input := ec2.ReleaseAddressInput{}
			if r.allocationID != "" && r.allocationID != "-" {
				input.AllocationId = aws.String(r.allocationID)
			} else if r.publicIP != "" && r.publicIP != "-" {
				input.PublicIp = aws.String(r.publicIP)
			} else {
				return nil // nothing to release
			}
			a.Logger.LogInfo("Releasing Elastic IP", map[string]any{
				"AllocationId": aws.ToString(input.AllocationId),
				"PublicIp":     aws.ToString(input.PublicIp),
			})
			_, err := a.AWSClient.EC2.ReleaseAddress(ctx, &input)
			return err
		},
		DeleteConcurrency: 5,
		MonthlyCost: func(r orphanElasticIP) cost.USD {
			return a.Pricing.ElasticIPMonth()
		},
	})
}

// stringOrDash returns the dereferenced value of p, or "-" if p is nil or empty.
func stringOrDash(p *string) string {
	if p == nil {
		return "-"
	}
	s := *p
	if s == "" {
		return "-"
	}
	return s
}
