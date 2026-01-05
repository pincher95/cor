/*
Copyright 2024 Elastic Scaler Contributors.

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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/pincher95/cor/pkg/utils"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// enisCmd represents the enis command
var enisCmd = &cobra.Command{
	Use:   "enis",
	Short: "Return orphaned ENIs (unattached network interfaces)",
	Long:  `Find and optionally delete network interfaces in "available" state (not attached).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout
		ctx := cmd.Context()

		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		additionalFlags := []flags.Flag{
			{Name: "filter-by-name", Type: "string"},
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

		return runENIsCmd(ctx, &prompterClient, output, awsClient, flagValues)
	},
}

func init() {
	enisCmd.Flags().String("filter-by-name", "*", "Filter ENIs by tag:Name (wildcards supported, e.g. 'foo*').")
}

func runENIsCmd(ctx context.Context, prompter *prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  *prompter,
		Output:    output,
	}
	return command.executeENIs(ctx, flagValues)
}

func (e *AWSCommand) executeENIs(ctx context.Context, flagValues *map[string]any) error {
	// If deleting, confirm up-front so we can stream without buffering IDs.
	doDelete := false
	if (*flagValues)["delete"].(bool) {
		confirm, err := e.Prompter.Confirm("Are you sure you want to proceed? (yes/no): ")
		if err != nil {
			e.Logger.LogError("Error during user prompt", err, nil, false)
			return err
		}
		if confirm == nil || !*confirm {
			e.Logger.LogInfo("Aborted.", nil)
			return nil
		}
		doDelete = true
	}

	eniChan := make(chan types.NetworkInterface, 50)
	resultsChan := make(chan table.Row, 50)

	g, ctx := errgroup.WithContext(ctx)

	// Describe ENIs (only unattached)
	g.Go(func() error {
		filters := []types.Filter{
			{Name: aws.String("status"), Values: []string{"available"}},
		}

		if filterByName, ok := (*flagValues)["filter-by-name"].(string); ok && filterByName != "" && filterByName != "*" {
			filters = append(filters, types.Filter{Name: aws.String("tag:Name"), Values: []string{filterByName}})
		}

		paginator := ec2.NewDescribeNetworkInterfacesPaginator(e.AWSClient.EC2, &ec2.DescribeNetworkInterfacesInput{
			Filters: filters,
		})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				close(eniChan)
				return err
			}
			for _, ni := range page.NetworkInterfaces {
				eniChan <- ni
			}
		}
		close(eniChan)
		return nil
	})

	for range NumGoroutines {
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case ni, ok := <-eniChan:
					if !ok {
						return nil
					}

					tagMap := utils.TagsToMap(ni.TagSet)
					name := "-"
					if t, ok := tagMap["Name"]; ok && t.Value != nil {
						name = *t.Value
					}

					eniID := aws.ToString(ni.NetworkInterfaceId)
					if eniID == "" {
						continue
					}

					ifType := string(ni.InterfaceType)
					if ifType == "" {
						ifType = "-"
					}
					status := string(ni.Status)
					if status == "" {
						status = "-"
					}
					requesterManaged := aws.ToBool(ni.RequesterManaged)
					desc := aws.ToString(ni.Description)
					if desc == "" {
						desc = "-"
					}
					vpcID := aws.ToString(ni.VpcId)
					if vpcID == "" {
						vpcID = "-"
					}
					subnetID := aws.ToString(ni.SubnetId)
					if subnetID == "" {
						subnetID = "-"
					}
					privateIP := aws.ToString(ni.PrivateIpAddress)
					if privateIP == "" {
						privateIP = "-"
					}

					resultsChan <- table.Row{name, eniID, ifType, status, requesterManaged, desc, vpcID, subnetID, privateIP}
				}
			}
		})
	}

	// Stream output + optional delete
	printDone := make(chan error, 1)
	go func() {
		stream := printer.NewStreamTable(e.Output, true, []string{"Name", "ENI ID", "Type", "Status", "RequesterManaged", "Description", "VPC ID", "Subnet ID", "Private IP"})
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
		defer stream.Close()

		for row := range resultsChan {
			stream.WriteRow(row...)

			if doDelete {
				// Name, ENI ID, Type, Status, RequesterManaged, ...
				if len(row) < 5 {
					continue
				}
				eniID, _ := row[1].(string)
				requesterManaged, _ := row[4].(bool)
				if requesterManaged {
					e.Logger.LogInfo("Skipping AWS-managed ENI", map[string]any{"NetworkInterfaceId": eniID})
					continue
				}
				if eniID == "" || eniID == "-" {
					continue
				}

				e.Logger.LogInfo("Deleting ENI", map[string]any{"NetworkInterfaceId": eniID})
				if _, err := e.AWSClient.EC2.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
					NetworkInterfaceId: aws.String(eniID),
				}); err != nil {
					printDone <- err
					return
				}
			}
		}
		printDone <- nil
	}()

	if err := g.Wait(); err != nil {
		e.Logger.LogError("Error during ENI processing", err, nil, false)
		return err
	}
	close(resultsChan)
	if err := <-printDone; err != nil {
		e.Logger.LogError("Error streaming/deleting ENIs", err, nil, false)
		return err
	}

	return nil
}

// Legacy pretty-table printer removed in favor of streaming output for low memory usage.
