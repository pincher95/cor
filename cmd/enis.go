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
	"io"
	"os"
	"strings"
	"sync"

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
			// New short flags
			{Name: "filter-by-enis", Type: "string"},
			{Name: "filter-by-vpc", Type: "string"},
			{Name: "filter-by-subnet", Type: "string"},
			{Name: "filter-by-sg", Type: "string"},
			{Name: "filter-by-type", Type: "string"},
			{Name: "filter-by-desc", Type: "string"},
			{Name: "filter-by-ip", Type: "string"},

			// Backwards-compatible aliases (hidden/deprecated in init())
			{Name: "filter-by-id-or-name", Type: "string"},
			{Name: "filter-by-vpc-id", Type: "string"},
			{Name: "filter-by-subnet-id", Type: "string"},
			{Name: "filter-by-security-group-id", Type: "string"},
			{Name: "filter-by-interface-type", Type: "string"},
			{Name: "filter-by-description", Type: "string"},
			{Name: "filter-by-private-ip", Type: "string"},
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
	// Preserve the original context for delete operations (avoid errgroup ctx cancellation).
	rootCtx := ctx

	collectDeletes := (*flagValues)["delete"].(bool)

	type nameCache struct {
		mu      sync.RWMutex
		vpcs    map[string]string
		subnets map[string]string
		sgs     map[string]string
	}
	cache := &nameCache{
		vpcs:    make(map[string]string),
		subnets: make(map[string]string),
		sgs:     make(map[string]string),
	}

	findNameTag := func(tags []types.Tag) string {
		for _, t := range tags {
			if aws.ToString(t.Key) == "Name" && t.Value != nil && aws.ToString(t.Value) != "" {
				return aws.ToString(t.Value)
			}
		}
		return "-"
	}

	formatIDAndName := func(id string, name string) string {
		if id == "" || id == "-" {
			return "-"
		}
		if name == "" || name == "-" {
			return id
		}
		return fmt.Sprintf("%s (%s)", id, name)
	}

	getVPCName := func(ctx context.Context, vpcID string) string {
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
			out = findNameTag(resp.Vpcs[0].Tags)
		}

		cache.mu.Lock()
		cache.vpcs[vpcID] = out
		cache.mu.Unlock()
		return out
	}

	getSubnetName := func(ctx context.Context, subnetID string) string {
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
			out = findNameTag(resp.Subnets[0].Tags)
		}

		cache.mu.Lock()
		cache.subnets[subnetID] = out
		cache.mu.Unlock()
		return out
	}

	ensureSGNames := func(ctx context.Context, sgIDs []string) {
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

	splitCSV := func(raw string) []string {
		raw = normalizeFilterValue(raw)
		if raw == "" {
			return nil
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" || p == "*" {
				continue
			}
			out = append(out, p)
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}

	getFlagString := func(name string) string {
		if v, ok := (*flagValues)[name].(string); ok {
			return v
		}
		return ""
	}

	mergeCSV := func(names ...string) []string {
		seen := make(map[string]struct{}, 8)
		out := make([]string, 0)
		for _, n := range names {
			for _, v := range splitCSV(getFlagString(n)) {
				if _, ok := seen[v]; ok {
					continue
				}
				seen[v] = struct{}{}
				out = append(out, v)
			}
		}
		return out
	}

	eniChan := make(chan types.NetworkInterface, 50)
	resultsChan := make(chan table.Row, 50)

	g, egCtx := errgroup.WithContext(ctx)

	// Describe ENIs (only unattached)
	g.Go(func() error {
		baseFilters := []types.Filter{
			{Name: aws.String("status"), Values: []string{"available"}},
		}

		if v := mergeCSV("filter-by-vpc", "filter-by-vpc-id"); len(v) > 0 {
			baseFilters = append(baseFilters, types.Filter{Name: aws.String("vpc-id"), Values: v})
		}
		if v := mergeCSV("filter-by-subnet", "filter-by-subnet-id"); len(v) > 0 {
			baseFilters = append(baseFilters, types.Filter{Name: aws.String("subnet-id"), Values: v})
		}
		if v := mergeCSV("filter-by-sg", "filter-by-security-group-id"); len(v) > 0 {
			baseFilters = append(baseFilters, types.Filter{Name: aws.String("group-id"), Values: v})
		}
		if v := mergeCSV("filter-by-type", "filter-by-interface-type"); len(v) > 0 {
			baseFilters = append(baseFilters, types.Filter{Name: aws.String("interface-type"), Values: v})
		}
		if v := mergeCSV("filter-by-ip", "filter-by-private-ip"); len(v) > 0 {
			baseFilters = append(baseFilters, types.Filter{Name: aws.String("private-ip-address"), Values: v})
		}
		desc := normalizeFilterValue(getFlagString("filter-by-desc"))
		if desc == "" {
			desc = normalizeFilterValue(getFlagString("filter-by-description"))
		}
		if desc != "" {
			// AWS supports wildcards in some EC2 filters (e.g. "*foo*"). Keep value as-is.
			baseFilters = append(baseFilters, types.Filter{Name: aws.String("description"), Values: []string{desc}})
		}

		// Back-compat: include --filter-by-name as a Name-tag filter input.
		names := splitCSV(getFlagString("filter-by-name"))

		// New UX: allow a single flag to match ENI ID OR tag:Name.
		ids := make([]string, 0)
		for _, tok := range mergeCSV("filter-by-enis", "filter-by-id-or-name") {
			if strings.HasPrefix(tok, "eni-") {
				ids = append(ids, tok)
			} else {
				names = append(names, tok)
			}
		}

		seen := make(map[string]struct{}, 128)
		emit := func(filters []types.Filter) error {
			paginator := ec2.NewDescribeNetworkInterfacesPaginator(e.AWSClient.EC2, &ec2.DescribeNetworkInterfacesInput{
				Filters: filters,
			})
			for paginator.HasMorePages() {
				page, err := paginator.NextPage(egCtx)
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
					eniChan <- ni
				}
			}
			return nil
		}

		// If neither id nor name query is provided, just use base filters.
		if len(ids) == 0 && len(names) == 0 {
			if err := emit(baseFilters); err != nil {
				close(eniChan)
				return err
			}
			close(eniChan)
			return nil
		}

		// OR behavior: (network-interface-id IN ids) OR (tag:Name IN names), with baseFilters ANDed.
		if len(ids) > 0 {
			f := append(append([]types.Filter{}, baseFilters...), types.Filter{Name: aws.String("network-interface-id"), Values: ids})
			if err := emit(f); err != nil {
				close(eniChan)
				return err
			}
		}
		if len(names) > 0 {
			f := append(append([]types.Filter{}, baseFilters...), types.Filter{Name: aws.String("tag:Name"), Values: names})
			if err := emit(f); err != nil {
				close(eniChan)
				return err
			}
		}

		close(eniChan)
		return nil
	})

	for range NumGoroutines {
		g.Go(func() error {
			for {
				select {
				case <-egCtx.Done():
					return egCtx.Err()
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

					sgIDs := "-"
					sgIDList := make([]string, 0, len(ni.Groups))
					if len(ni.Groups) > 0 {
						ids := make([]string, 0, len(ni.Groups))
						for _, g := range ni.Groups {
							if id := aws.ToString(g.GroupId); id != "" {
								ids = append(ids, id)
								sgIDList = append(sgIDList, id)
							}
						}
						if len(ids) > 0 {
							sgIDs = strings.Join(ids, ",")
						}
					}

					vpcName := getVPCName(egCtx, vpcID)
					subnetName := getSubnetName(egCtx, subnetID)

					vpcDisplay := formatIDAndName(vpcID, vpcName)
					subnetDisplay := formatIDAndName(subnetID, subnetName)

					sgDisplay := sgIDs
					if len(sgIDList) > 0 {
						ensureSGNames(egCtx, sgIDList)
						parts := make([]string, 0, len(sgIDList))
						cache.mu.RLock()
						for _, id := range sgIDList {
							parts = append(parts, formatIDAndName(id, cache.sgs[id]))
						}
						cache.mu.RUnlock()
						// One SG per line for readability.
						sgDisplay = strings.Join(parts, "\n")
					}

					resultsChan <- table.Row{name, eniID, ifType, status, requesterManaged, desc, vpcDisplay, subnetDisplay, privateIP, sgDisplay}
				}
			}
		})
	}

	// Stream output + optional delete
	printDone := make(chan []string, 1)
	go func() {
		stream := printer.NewStreamTable(e.Output, true, []string{"Name", "ENI ID", "Type", "Status", "RequesterManaged", "Description", "VPC", "Subnet", "Private IP", "Security Groups"})
		stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
		deleteIDs := make([]string, 0)
		finish := func() {
			stream.Close()
			printDone <- deleteIDs
		}

		for row := range resultsChan {
			stream.WriteRow(row...)

			if collectDeletes {
				// Name, ENI ID, Type, Status, RequesterManaged, ...
				if len(row) < 5 {
					continue
				}
				eniID, _ := row[1].(string)
				requesterManaged, _ := row[4].(bool)
				if requesterManaged {
					continue
				}
				if eniID == "" || eniID == "-" {
					continue
				}
				deleteIDs = append(deleteIDs, eniID)
			}
		}
		finish()
	}()

	if err := g.Wait(); err != nil {
		e.Logger.LogError("Error during ENI processing", err, nil, false)
		return err
	}
	close(resultsChan)
	deleteIDs := <-printDone
	if collectDeletes {
		if len(deleteIDs) == 0 {
			return nil
		}
		confirm, err := confirmDelete(e.Prompter, e.Logger)
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		for _, eniID := range deleteIDs {
			e.Logger.LogInfo("Deleting ENI", map[string]any{"NetworkInterfaceId": eniID})
			if _, err := e.AWSClient.EC2.DeleteNetworkInterface(rootCtx, &ec2.DeleteNetworkInterfaceInput{
				NetworkInterfaceId: aws.String(eniID),
			}); err != nil {
				e.Logger.LogError("Error deleting ENI", err, map[string]any{"NetworkInterfaceId": eniID}, false)
				return err
			}
		}
	}

	return nil
}

// Legacy pretty-table printer removed in favor of streaming output for low memory usage.
