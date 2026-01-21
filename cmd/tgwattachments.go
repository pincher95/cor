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
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
)

var tgwAttachmentsCmd = &cobra.Command{
	Use:   "tgwattachments",
	Short: "List and optionally delete Transit Gateway VPC attachments without route table association",
	Long:  `List Transit Gateway VPC attachments that are not associated with a route table and optionally delete them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout
		ctx := cmd.Context()

		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		additionalFlags := []flags.Flag{
			{Name: "include-associated", Type: "bool"},
			{Name: "include-non-vpc", Type: "bool"},
			{Name: "filter-by-resource", Type: "string"},
		}

		flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
		if err != nil {
			return err
		}

		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
			Profile:    aws.String((*flagValues)["profile"].(string)),
			Region:     aws.String((*flagValues)["region"].(string)),
		}
		cfg, err := handlers.NewConfig(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			return err
		}

		ec2Client := ec2.NewFromConfig(*cfg)
		awsClient := &handlers.AWSClientImpl{EC2: ec2Client}

		return runTGWAttachmentsCmd(ctx, prompterClient, output, awsClient, flagValues)
	},
}

func init() {
	tgwAttachmentsCmd.Flags().Bool("include-associated", false, "Include attachments associated with a route table.")
	tgwAttachmentsCmd.Flags().Bool("include-non-vpc", false, "Include non-VPC attachments (only VPC attachments can be deleted).")
	tgwAttachmentsCmd.Flags().String("filter-by-resource", "", "Filter by resource ID (substring match).")
}

func runTGWAttachmentsCmd(ctx context.Context, prompter prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  prompter,
		Output:    output,
	}
	return command.executeTGWAttachments(ctx, flagValues)
}

func (t *AWSCommand) executeTGWAttachments(ctx context.Context, flagValues *map[string]any) error {
	rootCtx := ctx
	collectDeletes := (*flagValues)["delete"].(bool)
	includeAssociated := (*flagValues)["include-associated"].(bool)
	includeNonVPC := (*flagValues)["include-non-vpc"].(bool)
	filterByResource := normalizeFilterValue((*flagValues)["filter-by-resource"].(string))

	stream := printer.NewStreamTable(t.Output, true, []string{"Attachment ID", "ResourceType", "ResourceId", "State", "AssocState"})
	stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
	defer stream.Close()

	deleteIDs := make([]string, 0)
	paginator := ec2.NewDescribeTransitGatewayAttachmentsPaginator(t.AWSClient.EC2, &ec2.DescribeTransitGatewayAttachmentsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, att := range page.TransitGatewayAttachments {
			resourceType := string(att.ResourceType)
			if resourceType != string(ec2types.TransitGatewayAttachmentResourceTypeVpc) && !includeNonVPC {
				continue
			}
			resourceID := aws.ToString(att.ResourceId)
			if filterByResource != "" && !strings.Contains(resourceID, filterByResource) {
				continue
			}
			assocState := "none"
			if att.Association != nil && att.Association.State != "" {
				assocState = string(att.Association.State)
			}
			if !includeAssociated && assocState == "associated" {
				continue
			}
			state := string(att.State)

			stream.WriteRow(aws.ToString(att.TransitGatewayAttachmentId), resourceType, resourceID, state, assocState)

			if collectDeletes && resourceType == string(ec2types.TransitGatewayAttachmentResourceTypeVpc) && assocState != "associated" {
				deleteIDs = append(deleteIDs, aws.ToString(att.TransitGatewayAttachmentId))
			}
		}
	}

	if collectDeletes {
		if len(deleteIDs) == 0 {
			return nil
		}
		confirm, err := confirmDelete(t.Prompter, t.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, id := range deleteIDs {
			t.Logger.LogInfo("Deleting TGW VPC attachment", map[string]any{"AttachmentId": id})
			if _, err := t.AWSClient.EC2.DeleteTransitGatewayVpcAttachment(rootCtx, &ec2.DeleteTransitGatewayVpcAttachmentInput{
				TransitGatewayAttachmentId: aws.String(id),
			}); err != nil {
				return err
			}
		}
	}

	return nil
}
