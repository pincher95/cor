/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Estimate total monthly cost across all orphan resource types",
	Long:  `Runs every cost-bearing resource scan and aggregates the estimated monthly orphan cost. List-only — never deletes. Use --all-regions to scan every enabled region.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{
					EC2:   ec2.NewFromConfig(*cfg),
					ELB:   elasticloadbalancingv2.NewFromConfig(*cfg),
					ELBv1: elasticloadbalancing.NewFromConfig(*cfg),
					RDS:   rds.NewFromConfig(*cfg),
					CWL:   cloudwatchlogs.NewFromConfig(*cfg),
					EFS:   efs.NewFromConfig(*cfg),
					R53:   route53.NewFromConfig(*cfg),
				}
			},
		}, (*AWSCommand).executeCostRollup)
	},
}

func init() {
	rootCmd.AddCommand(costCmd)
}

type rollupRow struct {
	resource string
	count    int
	monthly  cost.USD
}

func (a *AWSCommand) executeCostRollup(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	type rollupFn func(context.Context) (int, cost.USD, error)
	entries := []struct {
		label string
		fn    rollupFn
	}{
		{"EBS volumes", a.rollupVolumes},
		{"EBS snapshots", a.rollupSnapshots},
		{"Elastic IPs", a.rollupElasticIPs},
		{"NAT Gateways", a.rollupNatGateways},
		{"ELBv2", a.rollupElbv2},
		{"ELBv1 (Classic)", a.rollupElbv1},
		{"CloudWatch Log Groups", a.rollupLogs},
		{"EFS file systems", a.rollupEFS},
		{"Route53 hosted zones", a.rollupRoute53},
		{"VPC endpoints", a.rollupVPCEndpoints},
		{"VPN connections", a.rollupVPNConnections},
		{"RDS resources", a.rollupRDS},
	}

	// Run rollups concurrently. Index-keyed result slots avoid mutex overhead
	// and preserve the declared order in the final table.
	type result struct {
		row rollupRow
		ok  bool
	}
	slots := make([]result, len(entries))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i, e := range entries {
		g.Go(func() error {
			n, c, err := e.fn(gctx)
			if err != nil {
				a.Logger.LogError("rollup failed", err, map[string]any{"resource": e.label})
				return nil
			}
			slots[i] = result{row: rollupRow{resource: e.label, count: n, monthly: c}, ok: true}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	stream := printer.NewSink(globals.Format, a.Output, false, []string{"Resource", "Orphans", "Est $/mo"})
	defer stream.Close()
	rows := 0
	var grandTotal cost.USD
	for _, s := range slots {
		if !s.ok {
			continue
		}
		stream.WriteRow(s.row.resource, s.row.count, s.row.monthly)
		grandTotal += s.row.monthly
		rows++
	}
	stream.WriteRow(fmt.Sprintf("Total (%d types)", rows), "", grandTotal)
	return nil
}
