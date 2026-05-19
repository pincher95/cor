/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cost

import (
	"context"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"golang.org/x/sync/errgroup"
)

// Prorate returns actual × (allocated/total), or fallback when total ≤ 0.
// Used to slice an aggregate Cost Explorer bill across individual rows
// weighted by their allocated GB.
func Prorate(actual USD, allocated, total float64, fallback USD) USD {
	if total <= 0 {
		return fallback
	}
	return USD(float64(actual) * (allocated / total))
}

// CEActuals holds month-to-date totals from Cost Explorer for the resource
// types cor prorates against. Zero = not fetched / no usage.
type CEActuals struct {
	EBSSnapshotUSD    USD
	RDSManualSnapshot USD
	FetchedAt         time.Time
}

// FetchCEActuals pulls month-to-date totals for the SKUs cor prorates
// against. CE doesn't expose a "ResourceType=Snapshot" dimension, so the
// path is usage-type substring filtering. Errors on either query degrade
// the affected field to zero (the caller then falls back to static rates).
func FetchCEActuals(ctx context.Context, cfg aws.Config) (*CEActuals, error) {
	ceCfg := cfg
	ceCfg.Region = "us-east-1" // CE is us-east-1 only
	client := costexplorer.NewFromConfig(ceCfg)

	end := time.Now().UTC()
	start := time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, time.UTC)
	period := &cetypes.DateInterval{
		Start: aws.String(start.Format("2006-01-02")),
		End:   aws.String(end.Format("2006-01-02")),
	}

	queryUsageTypePrefix := func(ctx context.Context, prefix string) (USD, error) {
		out, err := client.GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
			TimePeriod:  period,
			Granularity: cetypes.GranularityMonthly,
			Metrics:     []string{"UnblendedCost"},
			Filter: &cetypes.Expression{
				Dimensions: &cetypes.DimensionValues{
					Key:          cetypes.DimensionUsageType,
					MatchOptions: []cetypes.MatchOption{cetypes.MatchOptionContains},
					Values:       []string{prefix},
				},
			},
		})
		if err != nil {
			return 0, err
		}
		var total USD
		for _, r := range out.ResultsByTime {
			if amt, ok := r.Total["UnblendedCost"]; ok && amt.Amount != nil {
				if v, perr := strconv.ParseFloat(*amt.Amount, 64); perr == nil {
					total += USD(v)
				}
			}
		}
		return total, nil
	}

	out := &CEActuals{FetchedAt: time.Now()}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		v, err := queryUsageTypePrefix(gctx, "EBS:SnapshotUsage")
		if err == nil {
			out.EBSSnapshotUSD = v
		}
		return nil
	})
	g.Go(func() error {
		v, err := queryUsageTypePrefix(gctx, "RDS:ChargedBackupUsage")
		if err == nil {
			out.RDSManualSnapshot = v
		}
		return nil
	})
	_ = g.Wait()
	return out, nil
}
