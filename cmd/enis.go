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
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanENI struct {
	name             string
	id               string
	interfaceType    string
	status           string
	requesterManaged bool
	description      string
	vpcDisplay       string
	subnetDisplay    string
	privateIP        string
	securityGroups   string
}

// enisCmd represents the enis command
var enisCmd = &cobra.Command{
	Use:   "enis",
	Short: "Return orphaned ENIs (unattached network interfaces)",
	Long:  `Find and optionally delete network interfaces in "available" state (not attached).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "filter-by-enis", Type: "string"},
				{Name: "filter-by-vpc", Type: "string"},
				{Name: "filter-by-subnet", Type: "string"},
				{Name: "filter-by-sg", Type: "string"},
				{Name: "filter-by-type", Type: "string"},
				{Name: "filter-by-desc", Type: "string"},
				{Name: "filter-by-ip", Type: "string"},
				{Name: "filter-by-id-or-name", Type: "string"},
				{Name: "filter-by-vpc-id", Type: "string"},
				{Name: "filter-by-subnet-id", Type: "string"},
				{Name: "filter-by-security-group-id", Type: "string"},
				{Name: "filter-by-interface-type", Type: "string"},
				{Name: "filter-by-description", Type: "string"},
				{Name: "filter-by-private-ip", Type: "string"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeENIs)
	},
}

func init() {
	enisCmd.Flags().String("filter-by-name", "", "Filter ENIs by tag:Name (empty = no filter).")

	// New short flags
	enisCmd.Flags().String("filter-by-enis", "", "Filter ENIs by ENI ID (eni-...) or tag:Name (comma-separated allowed; empty = no filter).")
	enisCmd.Flags().String("filter-by-vpc", "", "Filter ENIs by VPC ID (comma-separated allowed; empty = no filter).")
	enisCmd.Flags().String("filter-by-subnet", "", "Filter ENIs by Subnet ID (comma-separated allowed; empty = no filter).")
	enisCmd.Flags().String("filter-by-sg", "", "Filter ENIs by Security Group ID (comma-separated allowed; empty = no filter).")
	enisCmd.Flags().String("filter-by-type", "", "Filter ENIs by interface type (comma-separated allowed; e.g. interface, nat_gateway; empty = no filter).")
	enisCmd.Flags().String("filter-by-desc", "", "Filter ENIs by description (exact/wildcard match; empty = no filter).")
	enisCmd.Flags().String("filter-by-ip", "", "Filter ENIs by private IP (comma-separated allowed; empty = no filter).")

	// Backwards-compatible aliases (hidden/deprecated)
	enisCmd.Flags().String("filter-by-id-or-name", "", "DEPRECATED: use --filter-by-enis")
	enisCmd.Flags().String("filter-by-vpc-id", "", "DEPRECATED: use --filter-by-vpc")
	enisCmd.Flags().String("filter-by-subnet-id", "", "DEPRECATED: use --filter-by-subnet")
	enisCmd.Flags().String("filter-by-security-group-id", "", "DEPRECATED: use --filter-by-sg")
	enisCmd.Flags().String("filter-by-interface-type", "", "DEPRECATED: use --filter-by-type")
	enisCmd.Flags().String("filter-by-description", "", "DEPRECATED: use --filter-by-desc")
	enisCmd.Flags().String("filter-by-private-ip", "", "DEPRECATED: use --filter-by-ip")

	_ = enisCmd.Flags().MarkDeprecated("filter-by-id-or-name", "use --filter-by-enis")
	_ = enisCmd.Flags().MarkDeprecated("filter-by-vpc-id", "use --filter-by-vpc")
	_ = enisCmd.Flags().MarkDeprecated("filter-by-subnet-id", "use --filter-by-subnet")
	_ = enisCmd.Flags().MarkDeprecated("filter-by-security-group-id", "use --filter-by-sg")
	_ = enisCmd.Flags().MarkDeprecated("filter-by-interface-type", "use --filter-by-type")
	_ = enisCmd.Flags().MarkDeprecated("filter-by-description", "use --filter-by-desc")
	_ = enisCmd.Flags().MarkDeprecated("filter-by-private-ip", "use --filter-by-ip")

	_ = enisCmd.Flags().MarkHidden("filter-by-id-or-name")
	_ = enisCmd.Flags().MarkHidden("filter-by-vpc-id")
	_ = enisCmd.Flags().MarkHidden("filter-by-subnet-id")
	_ = enisCmd.Flags().MarkHidden("filter-by-security-group-id")
	_ = enisCmd.Flags().MarkHidden("filter-by-interface-type")
	_ = enisCmd.Flags().MarkHidden("filter-by-description")
	_ = enisCmd.Flags().MarkHidden("filter-by-private-ip")
}

func (e *AWSCommand) executeENIs(ctx context.Context, flagValues *map[string]any) error {
	cache := newAWSNameCache()

	filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))
	filterByENIs := mergeCSV(flagValues, "filter-by-enis", "filter-by-id-or-name")
	filterByVPC := mergeCSV(flagValues, "filter-by-vpc", "filter-by-vpc-id")
	filterBySubnet := mergeCSV(flagValues, "filter-by-subnet", "filter-by-subnet-id")
	filterBySG := mergeCSV(flagValues, "filter-by-sg", "filter-by-security-group-id")
	filterByType := mergeCSV(flagValues, "filter-by-type", "filter-by-interface-type")
	filterByIP := mergeCSV(flagValues, "filter-by-ip", "filter-by-private-ip")
	filterByDesc := normalizeFilterValue(getFlagString(flagValues, "filter-by-desc"))
	if filterByDesc == "" {
		filterByDesc = normalizeFilterValue(getFlagString(flagValues, "filter-by-description"))
	}

	return runOrphanPipeline(e, ctx, flagValues, OrphanPipeline[types.NetworkInterface, orphanENI]{
		Headers: []string{"Name", "ENI ID", "Type", "Status", "RequesterManaged", "Description", "VPC", "Subnet", "Private IP", "Security Groups"},
		List: func(ctx context.Context, emit func(types.NetworkInterface) error) error {
			baseFilters := []types.Filter{
				{Name: aws.String("status"), Values: []string{"available"}},
			}
			if len(filterByVPC) > 0 {
				baseFilters = append(baseFilters, types.Filter{Name: aws.String("vpc-id"), Values: filterByVPC})
			}
			if len(filterBySubnet) > 0 {
				baseFilters = append(baseFilters, types.Filter{Name: aws.String("subnet-id"), Values: filterBySubnet})
			}
			if len(filterBySG) > 0 {
				baseFilters = append(baseFilters, types.Filter{Name: aws.String("group-id"), Values: filterBySG})
			}
			if len(filterByType) > 0 {
				baseFilters = append(baseFilters, types.Filter{Name: aws.String("interface-type"), Values: filterByType})
			}
			if len(filterByIP) > 0 {
				baseFilters = append(baseFilters, types.Filter{Name: aws.String("private-ip-address"), Values: filterByIP})
			}
			if filterByDesc != "" {
				// AWS supports wildcards in some EC2 filters (e.g. "*foo*"). Keep value as-is.
				baseFilters = append(baseFilters, types.Filter{Name: aws.String("description"), Values: []string{filterByDesc}})
			}

			// Back-compat: include --filter-by-name as a Name-tag filter input.
			names := splitCSV(filterByName)

			// New UX: allow a single flag to match ENI ID OR tag:Name.
			ids := make([]string, 0)
			for _, tok := range filterByENIs {
				if strings.HasPrefix(tok, "eni-") {
					ids = append(ids, tok)
				} else {
					names = append(names, tok)
				}
			}

			seen := make(map[string]struct{}, 128)
			runPage := func(filters []types.Filter) error {
				paginator := ec2.NewDescribeNetworkInterfacesPaginator(e.AWSClient.EC2, &ec2.DescribeNetworkInterfacesInput{
					Filters: filters,
				})
				for paginator.HasMorePages() {
					page, err := paginator.NextPage(ctx)
					if err != nil {
						return err
					}
					for _, ni := range page.NetworkInterfaces {
						id := aws.ToString(ni.NetworkInterfaceId)
						if id != "" {
							if _, ok := seen[id]; ok {
								continue
							}
							seen[id] = struct{}{}
						}
						if err := emit(ni); err != nil {
							return err
						}
					}
				}
				return nil
			}

			// If neither id nor name query is provided, just use base filters.
			if len(ids) == 0 && len(names) == 0 {
				return runPage(baseFilters)
			}

			// OR behavior: (network-interface-id IN ids) OR (tag:Name IN names), with baseFilters ANDed.
			if len(ids) > 0 {
				f := append(append([]types.Filter{}, baseFilters...), types.Filter{Name: aws.String("network-interface-id"), Values: ids})
				if err := runPage(f); err != nil {
					return err
				}
			}
			if len(names) > 0 {
				f := append(append([]types.Filter{}, baseFilters...), types.Filter{Name: aws.String("tag:Name"), Values: names})
				if err := runPage(f); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(ctx context.Context, ni types.NetworkInterface) (*orphanENI, error) {
			eniID := aws.ToString(ni.NetworkInterfaceId)
			if eniID == "" {
				return nil, nil
			}

			name := "-"
			for _, t := range ni.TagSet {
				if aws.ToString(t.Key) == "Name" && t.Value != nil {
					name = *t.Value
					break
				}
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

			sgIDList := make([]string, 0, len(ni.Groups))
			for _, g := range ni.Groups {
				if id := aws.ToString(g.GroupId); id != "" {
					sgIDList = append(sgIDList, id)
				}
			}

			vpcName := e.getVPCName(ctx, cache, vpcID)
			subnetName := e.getSubnetName(ctx, cache, subnetID)
			e.ensureSGNames(ctx, cache, sgIDList)

			return &orphanENI{
				name:             name,
				id:               eniID,
				interfaceType:    ifType,
				status:           status,
				requesterManaged: requesterManaged,
				description:      desc,
				vpcDisplay:       formatIDAndName(vpcID, vpcName),
				subnetDisplay:    formatIDAndName(subnetID, subnetName),
				privateIP:        privateIP,
				securityGroups:   formatSGList(cache, sgIDList),
			}, nil
		},
		ToRow: func(r orphanENI) []any {
			return []any{r.name, r.id, r.interfaceType, r.status, r.requesterManaged, r.description, r.vpcDisplay, r.subnetDisplay, r.privateIP, r.securityGroups}
		},
		Delete: func(ctx context.Context, r orphanENI) error {
			if r.requesterManaged {
				return nil // skip AWS-managed ENIs (matches pre-refactor behavior)
			}
			if r.id == "" || r.id == "-" {
				return nil
			}
			e.Logger.LogInfo("Deleting ENI", map[string]any{"NetworkInterfaceId": r.id})
			_, err := e.AWSClient.EC2.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
				NetworkInterfaceId: aws.String(r.id),
			})
			return err
		},
	})
}

// findNameTag returns the value of the "Name" tag, or "" if absent/empty.
func findNameTag(tags []types.Tag) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == "Name" && t.Value != nil && aws.ToString(t.Value) != "" {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

// formatIDAndName renders "id (name)" when a name is present, "id" otherwise,
// and "-" when the id itself is empty.
func formatIDAndName(id string, name string) string {
	if id == "" || id == "-" {
		return "-"
	}
	if name == "" || name == "-" {
		return id
	}
	return fmt.Sprintf("%s (%s)", id, name)
}

// getVPCName returns the tag:Name of the given VPC, caching the result.
func (e *AWSCommand) getVPCName(ctx context.Context, cache *awsNameCache, vpcID string) string {
	if vpcID == "" || vpcID == "-" {
		return "-"
	}
	cache.mu.RLock()
	if n, ok := cache.vpcs[vpcID]; ok {
		cache.mu.RUnlock()
		return n
	}
	cache.mu.RUnlock()

	out := "-"
	resp, err := e.AWSClient.EC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
	if err == nil && len(resp.Vpcs) > 0 {
		if n := findNameTag(resp.Vpcs[0].Tags); n != "" {
			out = n
		}
	}

	cache.mu.Lock()
	cache.vpcs[vpcID] = out
	cache.mu.Unlock()
	return out
}

// getSubnetName returns the tag:Name of the given subnet, caching the result.
func (e *AWSCommand) getSubnetName(ctx context.Context, cache *awsNameCache, subnetID string) string {
	if subnetID == "" || subnetID == "-" {
		return "-"
	}
	cache.mu.RLock()
	if n, ok := cache.subnets[subnetID]; ok {
		cache.mu.RUnlock()
		return n
	}
	cache.mu.RUnlock()

	out := "-"
	resp, err := e.AWSClient.EC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{subnetID}})
	if err == nil && len(resp.Subnets) > 0 {
		if n := findNameTag(resp.Subnets[0].Tags); n != "" {
			out = n
		}
	}

	cache.mu.Lock()
	cache.subnets[subnetID] = out
	cache.mu.Unlock()
	return out
}

// ensureSGNames populates the security-group name cache for any of the given
// IDs that are not yet cached. On partial failure the missing IDs are cached
// as "-" so subsequent lookups do not re-issue the same failing request.
func (e *AWSCommand) ensureSGNames(ctx context.Context, cache *awsNameCache, sgIDs []string) {
	missing := make([]string, 0)
	cache.mu.RLock()
	for _, id := range sgIDs {
		if id == "" || id == "-" {
			continue
		}
		if _, ok := cache.sgs[id]; !ok {
			missing = append(missing, id)
		}
	}
	cache.mu.RUnlock()
	if len(missing) == 0 {
		return
	}
	resp, err := e.AWSClient.EC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: missing})
	cache.mu.Lock()
	defer cache.mu.Unlock()
	// Default missing to "-" to avoid repeated calls if Describe fails/partial.
	for _, id := range missing {
		if _, ok := cache.sgs[id]; !ok {
			cache.sgs[id] = "-"
		}
	}
	if err != nil {
		return
	}
	for _, sg := range resp.SecurityGroups {
		id := aws.ToString(sg.GroupId)
		if id == "" {
			continue
		}
		name := aws.ToString(sg.GroupName)
		if name == "" {
			name = "-"
		}
		cache.sgs[id] = name
	}
}

// formatSGList renders a newline-separated "id (Name)" list for the given
// security group IDs, matching the pre-refactor worker's inline formatting.
// Returns "-" when no IDs are provided.
func formatSGList(cache *awsNameCache, sgIDList []string) string {
	if len(sgIDList) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(sgIDList))
	cache.mu.RLock()
	for _, id := range sgIDList {
		parts = append(parts, formatIDAndName(id, cache.sgs[id]))
	}
	cache.mu.RUnlock()
	// One SG per line for readability.
	return strings.Join(parts, "\n")
}
