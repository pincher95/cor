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

type orphanTGWAttachment struct {
	id           string
	resourceType string
	resourceID   string
	state        string
	assocState   string
}

var tgwAttachmentsCmd = &cobra.Command{
	Use:   "tgwattachments",
	Short: "List and optionally delete Transit Gateway VPC attachments without route table association",
	Long:  `List Transit Gateway VPC attachments that are not associated with a route table and optionally delete them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "include-associated", Type: "bool"},
				{Name: "include-non-vpc", Type: "bool"},
				{Name: "filter-by-resource", Type: "string"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeTGWAttachments)
	},
}

func init() {
	tgwAttachmentsCmd.Flags().Bool("include-associated", false, "Include attachments associated with a route table.")
	tgwAttachmentsCmd.Flags().Bool("include-non-vpc", false, "Include non-VPC attachments (only VPC attachments can be deleted).")
	tgwAttachmentsCmd.Flags().String("filter-by-resource", "", "Filter by resource ID (substring match).")
}

func (a *AWSCommand) executeTGWAttachments(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	includeAssociated := (*extras)["include-associated"].(bool)
	includeNonVPC := (*extras)["include-non-vpc"].(bool)
	filterByResource := normalizeFilterValue((*extras)["filter-by-resource"].(string))

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[ec2types.TransitGatewayAttachment, orphanTGWAttachment]{
		Headers:       []string{"Attachment ID", "ResourceType", "ResourceId", "State", "AssocState"},
		ResourceLabel: "Transit Gateway attachments",
		List: func(ctx context.Context, emit func(ec2types.TransitGatewayAttachment) error) error {
			p := ec2.NewDescribeTransitGatewayAttachmentsPaginator(a.AWSClient.EC2, &ec2.DescribeTransitGatewayAttachmentsInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, att := range page.TransitGatewayAttachments {
					if err := emit(att); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, att ec2types.TransitGatewayAttachment) (*orphanTGWAttachment, error) {
			resourceType := string(att.ResourceType)
			if resourceType != string(ec2types.TransitGatewayAttachmentResourceTypeVpc) && !includeNonVPC {
				return nil, nil
			}
			resourceID := aws.ToString(att.ResourceId)
			if !matchesFilterValue(resourceID, filterByResource) {
				return nil, nil
			}
			assocState := "none"
			if att.Association != nil && att.Association.State != "" {
				assocState = string(att.Association.State)
			}
			if !includeAssociated && assocState == "associated" {
				return nil, nil
			}
			return &orphanTGWAttachment{
				id:           aws.ToString(att.TransitGatewayAttachmentId),
				resourceType: resourceType,
				resourceID:   resourceID,
				state:        string(att.State),
				assocState:   assocState,
			}, nil
		},
		ToRow: func(r orphanTGWAttachment) []any {
			return []any{r.id, r.resourceType, r.resourceID, r.state, r.assocState}
		},
		Delete: func(ctx context.Context, r orphanTGWAttachment) error {
			if r.resourceType != string(ec2types.TransitGatewayAttachmentResourceTypeVpc) || r.assocState == "associated" {
				return nil
			}
			a.Logger.LogInfo("Deleting TGW VPC attachment", map[string]any{"AttachmentId": r.id})
			_, err := a.AWSClient.EC2.DeleteTransitGatewayVpcAttachment(ctx, &ec2.DeleteTransitGatewayVpcAttachmentInput{
				TransitGatewayAttachmentId: aws.String(r.id),
			})
			return err
		},
		MonthlyCost: func(r orphanTGWAttachment) cost.USD {
			// $0.05/hour per VPC attachment; non-VPC types not modeled.
			if r.resourceType != string(ec2types.TransitGatewayAttachmentResourceTypeVpc) {
				return 0
			}
			return cost.USD(cost.HoursPerMonth) * 0.05
		},
	})
}
