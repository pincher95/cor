/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/aws/awstest"
	"github.com/spf13/viper"
)

func TestExecuteRDS_MultiProducerMerges(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	now := time.Now().UTC()
	calls := &awstest.Calls{}
	fakeCfg := awstest.Config(t, calls, awstest.Stubs{
		"DescribeDBInstances": &rds.DescribeDBInstancesOutput{
			DBInstances: []rdstypes.DBInstance{
				{
					DBInstanceIdentifier: aws.String("inst-stopped"),
					DBInstanceStatus:     aws.String("stopped"),
					InstanceCreateTime:   &now,
				},
				{
					DBInstanceIdentifier: aws.String("inst-running"),
					DBInstanceStatus:     aws.String("available"),
					InstanceCreateTime:   &now,
				},
			},
		},
		"DescribeDBSnapshots": &rds.DescribeDBSnapshotsOutput{
			DBSnapshots: []rdstypes.DBSnapshot{
				{
					DBSnapshotIdentifier: aws.String("snap-1"),
					Status:               aws.String("available"),
					SnapshotCreateTime:   &now,
				},
			},
		},
	})

	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{RDS: rds.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"rds", "--region", "us-east-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "inst-stopped") {
		t.Errorf("expected inst-stopped, got:\n%s", got)
	}
	if !strings.Contains(got, "snap-1") {
		t.Errorf("expected snap-1, got:\n%s", got)
	}
	if strings.Contains(got, "inst-running") {
		t.Errorf("should not include running instances:\n%s", got)
	}
	// DescribeDBInstances is called twice: once in PreScan to compute the
	// snapshot free-tier allowance (sum of allocated storage on running DBs),
	// once in the producer to list stopped instances.
	if calls.Count("DescribeDBInstances") != 2 {
		t.Errorf("expected 2 DescribeDBInstances (PreScan + producer), got %d", calls.Count("DescribeDBInstances"))
	}
	if calls.Count("DescribeDBSnapshots") != 1 {
		t.Errorf("expected 1 DescribeDBSnapshots, got %d", calls.Count("DescribeDBSnapshots"))
	}
}
