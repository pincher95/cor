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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// IdleSpec parameterizes a "is this resource idle?" CloudWatch query. Used by
// lambda/dynamodb/elasticache/opensearch (and any future cost-bearing
// resource where activity is the orphan signal).
type IdleSpec struct {
	Namespace  string
	MetricName string
	Dimensions []cwtypes.Dimension
	Window     time.Duration
	// Statistic defaults to Sum.
	Statistic cwtypes.Statistic
	// Threshold: a datapoint > Threshold means "active". Defaults to 0.
	Threshold float64
}

// IsIdle returns true when no datapoint in the requested window exceeds the
// threshold. A query error is returned as-is; callers decide whether to skip
// or fail the orphan.
func (a *AWSCommand) IsIdle(ctx context.Context, spec IdleSpec) (bool, error) {
	if a.AWSClient.CloudWatch == nil {
		return false, nil
	}
	end := time.Now()
	start := end.Add(-spec.Window)
	stat := spec.Statistic
	if stat == "" {
		stat = cwtypes.StatisticSum
	}
	// CloudWatch period must be in [60s, 86400s]; clamp to the nearest valid
	// bound. Inflating a sub-minute window to a full day was a bug — it
	// silently changed the semantics of the "is it idle?" question.
	period := min(max(int32(spec.Window.Seconds()), 60), 86400)
	out, err := a.AWSClient.CloudWatch.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String(spec.Namespace),
		MetricName: aws.String(spec.MetricName),
		Dimensions: spec.Dimensions,
		StartTime:  &start,
		EndTime:    &end,
		Period:     aws.Int32(period),
		Statistics: []cwtypes.Statistic{stat},
	})
	if err != nil {
		return false, err
	}
	for _, dp := range out.Datapoints {
		var v float64
		switch stat {
		case cwtypes.StatisticSum:
			v = aws.ToFloat64(dp.Sum)
		case cwtypes.StatisticAverage:
			v = aws.ToFloat64(dp.Average)
		case cwtypes.StatisticMaximum:
			v = aws.ToFloat64(dp.Maximum)
		}
		if v > spec.Threshold {
			return false, nil
		}
	}
	return true, nil
}
