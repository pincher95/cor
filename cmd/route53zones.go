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
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
)

var route53ZonesCmd = &cobra.Command{
	Use:   "route53zones",
	Short: "List and optionally delete Route53 hosted zones with only NS/SOA records",
	Long:  `List hosted zones that appear empty (only NS/SOA records) and optionally delete them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "include-non-empty", Type: "bool"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{R53: route53.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeRoute53Zones)
	},
}

func init() {
	route53ZonesCmd.Flags().String("filter-by-name", "", "Filter hosted zones by name (substring match).")
	route53ZonesCmd.Flags().Bool("include-non-empty", false, "Include zones that have records beyond NS/SOA.")
}

func (r *AWSCommand) executeRoute53Zones(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	collectDeletes := globals.Delete
	includeNonEmpty := (*extras)["include-non-empty"].(bool)
	filterByName := normalizeFilterValue((*extras)["filter-by-name"].(string))

	stream := printer.NewStreamTable(r.Output, true, []string{"Zone Name", "Zone ID", "RecordSets", "Private", "Orphan"})
	stream.SetSort(globals.SortBy, globals.SortDesc)
	defer stream.Close()

	deleteIDs := make([]string, 0)
	paginator := route53.NewListHostedZonesPaginator(r.AWSClient.R53, &route53.ListHostedZonesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, zone := range page.HostedZones {
			name := strings.TrimSuffix(aws.ToString(zone.Name), ".")
			if filterByName != "" && !strings.Contains(name, filterByName) {
				continue
			}

			orphan, err := r.isRoute53ZoneOrphan(ctx, zone.Id)
			if err != nil {
				return err
			}
			if !includeNonEmpty && !orphan {
				continue
			}

			recordCount := aws.ToInt64(zone.ResourceRecordSetCount)
			private := false
			if zone.Config != nil {
				private = zone.Config.PrivateZone
			}
			zoneID := strings.TrimPrefix(aws.ToString(zone.Id), "/hostedzone/")

			stream.WriteRow(name, zoneID, recordCount, private, orphan)
			if collectDeletes && orphan {
				deleteIDs = append(deleteIDs, zoneID)
			}
		}
	}

	if collectDeletes {
		if len(deleteIDs) == 0 {
			return nil
		}
		confirm, err := confirmDelete(r.Prompter, r.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, zoneID := range deleteIDs {
			r.Logger.LogInfo("Deleting hosted zone", map[string]any{"ZoneId": zoneID})
			if _, err := r.AWSClient.R53.DeleteHostedZone(ctx, &route53.DeleteHostedZoneInput{
				Id: aws.String(zoneID),
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *AWSCommand) isRoute53ZoneOrphan(ctx context.Context, zoneID *string) (bool, error) {
	id := strings.TrimPrefix(aws.ToString(zoneID), "/hostedzone/")
	out, err := r.AWSClient.R53.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(id),
		MaxItems:     aws.Int32(3),
	})
	if err != nil {
		return false, err
	}
	if len(out.ResourceRecordSets) > 2 {
		return false, nil
	}
	for _, rr := range out.ResourceRecordSets {
		if rr.Type != r53types.RRTypeNs && rr.Type != r53types.RRTypeSoa {
			return false, nil
		}
	}
	return true, nil
}
