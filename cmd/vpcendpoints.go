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
	"github.com/pincher95/cor/pkg/utils"
	"github.com/spf13/cobra"
)

var vpcEndpointsCmd = &cobra.Command{
	Use:   "vpcendpoints",
	Short: "List and optionally delete orphan interface VPC endpoints",
	Long:  `List interface VPC endpoints that have no network interfaces and optionally delete them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout
		ctx := cmd.Context()

		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		additionalFlags := []flags.Flag{
			{Name: "filter-by-service", Type: "string"},
			{Name: "include-attached", Type: "bool"},
			{Name: "include-non-interface", Type: "bool"},
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

		return runVPCEndpointsCmd(ctx, prompterClient, output, awsClient, flagValues)
	},
}

func init() {
	vpcEndpointsCmd.Flags().String("filter-by-service", "", "Filter by VPC endpoint service name (substring match).")
	vpcEndpointsCmd.Flags().Bool("include-attached", false, "Include endpoints that have network interfaces attached.")
	vpcEndpointsCmd.Flags().Bool("include-non-interface", false, "Include non-interface endpoints (gateway endpoints are typically free).")
}

func runVPCEndpointsCmd(ctx context.Context, prompter prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  prompter,
		Output:    output,
	}
	return command.executeVPCEndpoints(ctx, flagValues)
}

func (v *AWSCommand) executeVPCEndpoints(ctx context.Context, flagValues *map[string]any) error {
	rootCtx := ctx
	collectDeletes := (*flagValues)["delete"].(bool)
	filterByService := normalizeFilterValue((*flagValues)["filter-by-service"].(string))
	includeAttached := (*flagValues)["include-attached"].(bool)
	includeNonInterface := (*flagValues)["include-non-interface"].(bool)

	stream := printer.NewStreamTable(v.Output, true, []string{"Name", "Endpoint ID", "Service", "Type", "State", "ENIs"})
	stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
	defer stream.Close()

	deleteIDs := make([]string, 0)

	filters := []ec2types.Filter{}
	if !includeNonInterface {
		filters = append(filters, ec2types.Filter{
			Name:   aws.String("vpc-endpoint-type"),
			Values: []string{string(ec2types.VpcEndpointTypeInterface)},
		})
	}

	paginator := ec2.NewDescribeVpcEndpointsPaginator(v.AWSClient.EC2, &ec2.DescribeVpcEndpointsInput{
		Filters: filters,
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, ep := range page.VpcEndpoints {
			service := aws.ToString(ep.ServiceName)
			if filterByService != "" && !strings.Contains(service, filterByService) {
				continue
			}
			eniCount := 0
			if ep.NetworkInterfaceIds != nil {
				eniCount = len(ep.NetworkInterfaceIds)
			}
			if !includeAttached && eniCount > 0 {
				continue
			}
			name := "-"
			tagMap := utils.TagsToMap(ep.Tags)
			if t, ok := tagMap["Name"]; ok && t.Value != nil {
				name = *t.Value
			}

			stream.WriteRow(name, aws.ToString(ep.VpcEndpointId), service, string(ep.VpcEndpointType), string(ep.State), eniCount)
			if collectDeletes && eniCount == 0 {
				deleteIDs = append(deleteIDs, aws.ToString(ep.VpcEndpointId))
			}
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
		_, err = v.AWSClient.EC2.DeleteVpcEndpoints(rootCtx, &ec2.DeleteVpcEndpointsInput{
			VpcEndpointIds: deleteIDs,
		})
		if err != nil {
			return err
		}
	}

	return nil
}
