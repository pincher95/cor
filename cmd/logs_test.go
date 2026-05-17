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
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/aws/awstest"
	"github.com/spf13/viper"
)

// TestExecuteLogs_DeleteUsesRootContext is the regression test for Phase 0.1.
// The previous implementation shadowed the outer ctx with the errgroup ctx and
// used it for DeleteLogGroup; this test ensures all candidates are deleted.
func TestExecuteLogs_DeleteUsesRootContext(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	calls := &awstest.Calls{}
	fakeCfg := awstest.Config(t, calls, awstest.Stubs{
		"DescribeLogGroups": &cloudwatchlogs.DescribeLogGroupsOutput{
			LogGroups: []cwltypes.LogGroup{
				{LogGroupName: aws.String("lg-1"), StoredBytes: aws.Int64(100)},
				{LogGroupName: aws.String("lg-2"), StoredBytes: aws.Int64(200)},
				{LogGroupName: aws.String("lg-3"), StoredBytes: aws.Int64(300)},
			},
		},
		"DeleteLogGroup": &cloudwatchlogs.DeleteLogGroupOutput{},
	})
	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{CWL: cloudwatchlogs.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"logs", "--region", "us-east-1", "--delete", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}
	if calls.Count("DeleteLogGroup") != 3 {
		t.Errorf("expected 3 DeleteLogGroup calls, got %d (rootCtx invariant likely broken)", calls.Count("DeleteLogGroup"))
	}
}
