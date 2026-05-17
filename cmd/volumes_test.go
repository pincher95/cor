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
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/aws/awstest"
	"github.com/spf13/viper"
)

func restoreRunnerSeams(prevCfg func(context.Context, handlers.CloudConfig, string, bool, bool) (*aws.Config, error),
	prevBuild func(CommandSetup, *aws.Config) *handlers.AWSClientImpl,
) {
	newConfigFn = prevCfg
	buildClientsFn = prevBuild
}

func TestExecuteVolumes_FiltersAvailableOnly(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	calls := &awstest.Calls{}
	fakeCfg := awstest.Config(t, calls, awstest.Stubs{
		"DescribeVolumes": &ec2.DescribeVolumesOutput{
			Volumes: []ec2types.Volume{
				{
					VolumeId:   aws.String("vol-aaa"),
					Size:       aws.Int32(8),
					SnapshotId: aws.String("snap-1"),
					Tags:       []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("alpha")}},
				},
				{
					VolumeId:   aws.String("vol-bbb"),
					Size:       aws.Int32(16),
					SnapshotId: aws.String("snap-2"),
				},
			},
		},
	})

	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"volumes", "--region", "us-east-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "vol-aaa") || !strings.Contains(got, "vol-bbb") {
		t.Errorf("expected both vol-aaa and vol-bbb in output, got:\n%s", got)
	}
	if !strings.Contains(got, "Total") {
		t.Errorf("expected Finalize Total footer, got:\n%s", got)
	}
	if calls.Count("DescribeVolumes") != 1 {
		t.Errorf("expected exactly 1 DescribeVolumes call, got %d", calls.Count("DescribeVolumes"))
	}
}

func TestExecuteVolumes_DeleteWithYesIssuesDeleteCalls(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	calls := &awstest.Calls{}
	fakeCfg := awstest.Config(t, calls, awstest.Stubs{
		"DescribeVolumes": &ec2.DescribeVolumesOutput{
			Volumes: []ec2types.Volume{
				{VolumeId: aws.String("vol-1"), Size: aws.Int32(8), SnapshotId: aws.String("snap-1")},
				{VolumeId: aws.String("vol-2"), Size: aws.Int32(16), SnapshotId: aws.String("snap-2")},
			},
		},
		"DeleteVolume": &ec2.DeleteVolumeOutput{},
	})

	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"volumes", "--region", "us-east-1", "--delete", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}
	if calls.Count("DeleteVolume") != 2 {
		t.Errorf("expected 2 DeleteVolume calls, got %d", calls.Count("DeleteVolume"))
	}
}

func TestExecuteVolumes_DescribeErrorPropagates(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	sentinel := errors.New("api boom")
	fakeCfg := awstest.Config(t, nil, awstest.Stubs{
		"DescribeVolumes": func(_ context.Context, _ any) (any, error) {
			return nil, sentinel
		},
	})
	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"volumes", "--region", "us-east-1"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from describe failure")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error chain, got %v", err)
	}
}
