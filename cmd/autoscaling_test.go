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
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/aws/awstest"
	"github.com/spf13/viper"
)

func TestExecuteAutoscaling_StrictOrphanCriteria(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	zero := int32(0)
	one := int32(1)
	calls := &awstest.Calls{}
	fakeCfg := awstest.Config(t, calls, awstest.Stubs{
		"DescribeAutoScalingGroups": &autoscaling.DescribeAutoScalingGroupsOutput{
			AutoScalingGroups: []autoscalingtypes.AutoScalingGroup{
				{
					AutoScalingGroupName: aws.String("asg-empty"),
					MinSize:              &zero,
					MaxSize:              &zero,
					DesiredCapacity:      &zero,
				},
				{
					AutoScalingGroupName: aws.String("asg-has-min"),
					MinSize:              &one,
					MaxSize:              &one,
					DesiredCapacity:      &zero,
				},
				{
					AutoScalingGroupName: aws.String("asg-has-instances"),
					MinSize:              &zero,
					MaxSize:              &one,
					DesiredCapacity:      &zero,
					Instances:            []autoscalingtypes.Instance{{InstanceId: aws.String("i-aaa")}},
				},
				{
					AutoScalingGroupName: aws.String("asg-has-lb"),
					MinSize:              &zero,
					MaxSize:              &one,
					DesiredCapacity:      &zero,
					LoadBalancerNames:    []string{"lb-1"},
				},
			},
		},
	})
	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{ASG: autoscaling.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"autoscaling", "--region", "us-east-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "asg-empty") {
		t.Errorf("expected asg-empty in output, got:\n%s", got)
	}
	for _, banned := range []string{"asg-has-min", "asg-has-instances", "asg-has-lb"} {
		if strings.Contains(got, banned) {
			t.Errorf("did not expect %q in orphan output, got:\n%s", banned, got)
		}
	}
}
