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
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/aws/awstest"
	"github.com/spf13/viper"
)

func TestExecuteIAMPolicies_OnlyUnattachedSurface(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	calls := &awstest.Calls{}
	fakeCfg := awstest.Config(t, calls, awstest.Stubs{
		"ListPolicies": &iam.ListPoliciesOutput{
			Policies: []iamtypes.Policy{
				{
					PolicyName:                    aws.String("orphan-pol"),
					Arn:                           aws.String("arn:aws:iam::111:policy/orphan-pol"),
					Path:                          aws.String("/"),
					AttachmentCount:               aws.Int32(0),
					PermissionsBoundaryUsageCount: aws.Int32(0),
					CreateDate:                    aws.Time(time.Now().Add(-100 * 24 * time.Hour)),
				},
				{
					PolicyName:                    aws.String("attached-pol"),
					Arn:                           aws.String("arn:aws:iam::111:policy/attached-pol"),
					Path:                          aws.String("/"),
					AttachmentCount:               aws.Int32(3),
					PermissionsBoundaryUsageCount: aws.Int32(0),
					CreateDate:                    aws.Time(time.Now().Add(-100 * 24 * time.Hour)),
				},
				{
					PolicyName:                    aws.String("boundary-pol"),
					Arn:                           aws.String("arn:aws:iam::111:policy/boundary-pol"),
					Path:                          aws.String("/"),
					AttachmentCount:               aws.Int32(0),
					PermissionsBoundaryUsageCount: aws.Int32(2),
					CreateDate:                    aws.Time(time.Now().Add(-100 * 24 * time.Hour)),
				},
			},
		},
	})

	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{IAM: iam.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"iampolicies", "--region", "us-east-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "orphan-pol") {
		t.Errorf("expected orphan-pol in output, got:\n%s", got)
	}
	if strings.Contains(got, "attached-pol") || strings.Contains(got, "boundary-pol") {
		t.Errorf("attached/boundary policies must not surface, got:\n%s", got)
	}
	if calls.Count("ListPolicies") != 1 {
		t.Errorf("expected 1 ListPolicies call, got %d", calls.Count("ListPolicies"))
	}
}

func TestExecuteIAMPolicies_MinAgeFiltersYoung(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	fakeCfg := awstest.Config(t, nil, awstest.Stubs{
		"ListPolicies": &iam.ListPoliciesOutput{
			Policies: []iamtypes.Policy{
				{
					PolicyName:                    aws.String("fresh-pol"),
					Arn:                           aws.String("arn:aws:iam::111:policy/fresh-pol"),
					Path:                          aws.String("/"),
					AttachmentCount:               aws.Int32(0),
					PermissionsBoundaryUsageCount: aws.Int32(0),
					CreateDate:                    aws.Time(time.Now()),
				},
			},
		},
	})
	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{IAM: iam.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"iampolicies", "--region", "us-east-1", "--min-age-days", "30"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}
	if strings.Contains(out.String(), "fresh-pol") {
		t.Errorf("policy younger than --min-age-days must not surface, got:\n%s", out.String())
	}
}

func TestExecuteIAMPolicies_DeleteRemovesVersionsThenPolicy(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	calls := &awstest.Calls{}
	fakeCfg := awstest.Config(t, calls, awstest.Stubs{
		"ListPolicies": &iam.ListPoliciesOutput{
			Policies: []iamtypes.Policy{
				{
					PolicyName:                    aws.String("orphan-pol"),
					Arn:                           aws.String("arn:aws:iam::111:policy/orphan-pol"),
					Path:                          aws.String("/"),
					AttachmentCount:               aws.Int32(0),
					PermissionsBoundaryUsageCount: aws.Int32(0),
					CreateDate:                    aws.Time(time.Now().Add(-100 * 24 * time.Hour)),
				},
			},
		},
		"ListPolicyVersions": &iam.ListPolicyVersionsOutput{
			Versions: []iamtypes.PolicyVersion{
				{VersionId: aws.String("v1"), IsDefaultVersion: false},
				{VersionId: aws.String("v2"), IsDefaultVersion: false},
				{VersionId: aws.String("v3"), IsDefaultVersion: true},
			},
		},
		"DeletePolicyVersion": &iam.DeletePolicyVersionOutput{},
		"DeletePolicy":        &iam.DeletePolicyOutput{},
	})
	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{IAM: iam.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"iampolicies", "--region", "us-east-1", "--delete", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}
	if calls.Count("DeletePolicyVersion") != 2 {
		t.Errorf("expected 2 DeletePolicyVersion calls (non-default only), got %d", calls.Count("DeletePolicyVersion"))
	}
	if calls.Count("DeletePolicy") != 1 {
		t.Errorf("expected 1 DeletePolicy call, got %d", calls.Count("DeletePolicy"))
	}
}

func TestExecuteIAMPolicies_RequireTagGate(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	calls := &awstest.Calls{}
	fakeCfg := awstest.Config(t, calls, awstest.Stubs{
		"ListPolicies": &iam.ListPoliciesOutput{
			Policies: []iamtypes.Policy{
				{PolicyName: aws.String("keep-pol"), Arn: aws.String("arn:aws:iam::111:policy/keep-pol"), Path: aws.String("/"), AttachmentCount: aws.Int32(0), PermissionsBoundaryUsageCount: aws.Int32(0), CreateDate: aws.Time(time.Now().Add(-100 * 24 * time.Hour))},
				{PolicyName: aws.String("drop-pol"), Arn: aws.String("arn:aws:iam::111:policy/drop-pol"), Path: aws.String("/"), AttachmentCount: aws.Int32(0), PermissionsBoundaryUsageCount: aws.Int32(0), CreateDate: aws.Time(time.Now().Add(-100 * 24 * time.Hour))},
			},
		},
		"ListPolicyTags": func(_ context.Context, in any) (any, error) {
			li := in.(*iam.ListPolicyTagsInput)
			if aws.ToString(li.PolicyArn) == "arn:aws:iam::111:policy/keep-pol" {
				return &iam.ListPolicyTagsOutput{Tags: []iamtypes.Tag{{Key: aws.String("cor-managed"), Value: aws.String("true")}}}, nil
			}
			return &iam.ListPolicyTagsOutput{}, nil
		},
	})
	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{IAM: iam.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"iampolicies", "--region", "us-east-1", "--require-tag", "cor-managed=true"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "keep-pol") {
		t.Errorf("expected keep-pol (matching tag) to surface, got:\n%s", got)
	}
	if strings.Contains(got, "drop-pol") {
		t.Errorf("drop-pol lacks the required tag and must be gated out, got:\n%s", got)
	}
	if calls.Count("ListPolicyTags") != 2 {
		t.Errorf("expected ListPolicyTags called once per candidate (2), got %d", calls.Count("ListPolicyTags"))
	}
}
