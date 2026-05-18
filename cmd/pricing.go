/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
	"github.com/pincher95/cor/pkg/cost"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
)

var pricingCmd = &cobra.Command{
	Use:   "pricing",
	Short: "Inspect embedded pricing rates used for cost estimates",
}

var pricingShowCmd = &cobra.Command{
	Use:   "show [region]",
	Short: "Print the embedded rate table for a region (default: us-east-1)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		region := "us-east-1"
		if len(args) == 1 {
			region = args[0]
		}
		p := cost.New(region)
		stream := printer.NewSink("table", cmd.OutOrStdout(), false, []string{"SKU", "Rate", "Unit"})
		defer stream.Close()
		for _, e := range p.Entries() {
			stream.WriteRow(e.SKU, e.Rate, e.Unit)
		}
		return nil
	},
}

func init() {
	pricingCmd.AddCommand(pricingShowCmd)
	rootCmd.AddCommand(pricingCmd)
}
