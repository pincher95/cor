/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
)

var pricingCmd = &cobra.Command{
	Use:   "pricing",
	Short: "Inspect or refresh AWS rate tables used for cost estimates",
	Long: `cor ships with a hardcoded us-east-1 rate table — fine offline but wrong
for other regions. ` + "`pricing refresh`" + ` pulls live list prices from the AWS
Pricing API and caches them per-region at ~/.cor/prices/<region>.json.
Subsequent cor runs use the cache automatically.`,
}

var pricingShowCmd = &cobra.Command{
	Use:   "show [region]",
	Short: "Print the rate table currently in effect for a region (cache or hardcoded)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		region := "us-east-1"
		if len(args) == 1 {
			region = args[0]
		}
		status, _ := cost.CacheStatusFor(region)
		source := "bundled defaults"
		if status.Present {
			source = fmt.Sprintf("cache (fetched %s)", status.FetchedAt.UTC().Format("2006-01-02 15:04 UTC"))
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "region=%s source=%s\n\n", region, source)

		p := cost.New(region)
		stream := printer.NewSink("table", cmd.OutOrStdout(), false, []string{"SKU", "Rate", "Unit"})
		defer stream.Close()
		for _, e := range p.Entries() {
			stream.WriteRow(e.SKU, e.Rate, e.Unit)
		}
		return nil
	},
}

var pricingRefreshCmd = &cobra.Command{
	Use:   "refresh",
	Short: "Pull list prices from AWS Pricing API into the local cache",
	RunE:  runPricingRefresh,
}

func init() {
	pricingCmd.AddCommand(pricingShowCmd)
	pricingCmd.AddCommand(pricingRefreshCmd)
	rootCmd.AddCommand(pricingCmd)
}

func runPricingRefresh(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	authMethod, _ := cmd.Flags().GetString("auth-method")
	profile, _ := cmd.Flags().GetString("profile")
	region, _ := cmd.Flags().GetString("region")
	allRegions, _ := cmd.Flags().GetBool("all-regions")

	base, err := handlers.NewConfig(ctx, handlers.CloudConfig{
		AuthMethod: aws.String(authMethod),
		Profile:    aws.String(profile),
		Region:     aws.String(region),
	}, "UTC", true, true)
	if err != nil {
		return err
	}

	regions := []string{region}
	if allRegions {
		regions, err = enabledRegions(ctx, ec2.NewFromConfig(*base))
		if err != nil {
			return fmt.Errorf("list regions: %w", err)
		}
	}

	for _, r := range regions {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "refreshing %s … ", r)
		rt, err := cost.RefreshRegion(ctx, *base, r)
		if err != nil {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "partial (%v)\n", err)
		} else {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "ok")
		}
		// RefreshRegion always returns a populated table even on partial
		// failure — cache it so subsequent runs see whatever we got.
		if rt == nil {
			continue
		}
		if err := cost.SaveCache(r, rt); err != nil {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  save: %v\n", err)
		}
	}
	return nil
}
