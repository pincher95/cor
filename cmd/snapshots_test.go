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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/aws/awstest"
	"github.com/spf13/viper"
)

func TestExecuteSnapshots_PreScanFiltersUsedSnapshots(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	calls := &awstest.Calls{}
	fakeCfg := awstest.Config(t, calls, awstest.Stubs{
		"DescribeImages": &ec2.DescribeImagesOutput{
			Images: []ec2types.Image{
				{
					ImageId: aws.String("ami-1"),
					BlockDeviceMappings: []ec2types.BlockDeviceMapping{
						{Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String("snap-used-by-image")}},
					},
				},
			},
		},
		"DescribeVolumes": &ec2.DescribeVolumesOutput{
			Volumes: []ec2types.Volume{
				{VolumeId: aws.String("vol-1"), SnapshotId: aws.String("snap-used-by-volume")},
			},
		},
		"DescribeSnapshots": &ec2.DescribeSnapshotsOutput{
			Snapshots: []ec2types.Snapshot{
				{SnapshotId: aws.String("snap-orphan"), VolumeSize: aws.Int32(8)},
				{SnapshotId: aws.String("snap-used-by-image"), VolumeSize: aws.Int32(4)},
				{SnapshotId: aws.String("snap-used-by-volume"), VolumeSize: aws.Int32(16)},
				{
					SnapshotId:  aws.String("snap-lifecycle"),
					VolumeSize:  aws.Int32(32),
					Description: aws.String("Created for policy abc"),
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
	cmd := rootCmdForTest(out, []string{"snapshots", "--region", "us-east-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "snap-orphan") {
		t.Errorf("expected snap-orphan in output, got:\n%s", got)
	}
	for _, banned := range []string{"snap-used-by-image", "snap-used-by-volume", "snap-lifecycle"} {
		if strings.Contains(got, banned) {
			t.Errorf("did not expect %q in orphan output, got:\n%s", banned, got)
		}
	}
	if !strings.Contains(got, "Total") {
		t.Errorf("expected Total finalize row, got:\n%s", got)
	}
}
