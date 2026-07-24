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

func firstIndex(names []string, op string) int {
	for i, n := range names {
		if n == op {
			return i
		}
	}
	return -1
}

func TestExecuteIAMRoles_SkipsServiceLinkedAndYoung(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	calls := &awstest.Calls{}
	fakeCfg := awstest.Config(t, calls, awstest.Stubs{
		"ListRoles": &iam.ListRolesOutput{
			Roles: []iamtypes.Role{
				{RoleName: aws.String("aws-svc"), Path: aws.String("/aws-service-role/elasticbeanstalk/"), Arn: aws.String("arn:aws:iam::111:role/aws-svc")},
				{RoleName: aws.String("young"), Path: aws.String("/"), Arn: aws.String("arn:aws:iam::111:role/young")},
				{RoleName: aws.String("old"), Path: aws.String("/"), Arn: aws.String("arn:aws:iam::111:role/old")},
			},
		},
		"GetRole": func(_ context.Context, in any) (any, error) {
			gi := in.(*iam.GetRoleInput)
			switch aws.ToString(gi.RoleName) {
			case "old":
				return &iam.GetRoleOutput{Role: &iamtypes.Role{
					RoleName:   aws.String("old"),
					Arn:        aws.String("arn:aws:iam::111:role/old"),
					Path:       aws.String("/"),
					CreateDate: aws.Time(time.Now().Add(-200 * 24 * time.Hour)),
				}}, nil
			default:
				return &iam.GetRoleOutput{Role: &iamtypes.Role{
					RoleName:   gi.RoleName,
					Arn:        aws.String("arn:aws:iam::111:role/" + aws.ToString(gi.RoleName)),
					Path:       aws.String("/"),
					CreateDate: aws.Time(time.Now()),
				}}, nil
			}
		},
		"ListAttachedRolePolicies":    &iam.ListAttachedRolePoliciesOutput{},
		"ListRolePolicies":            &iam.ListRolePoliciesOutput{},
		"ListInstanceProfilesForRole": &iam.ListInstanceProfilesForRoleOutput{},
	})
	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{IAM: iam.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"iamroles", "--region", "us-east-1", "--max-unused-days", "90"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "old") {
		t.Errorf("expected old role to surface, got:\n%s", got)
	}
	if strings.Contains(got, "young") || strings.Contains(got, "aws-svc") {
		t.Errorf("young/service-linked roles must not surface, got:\n%s", got)
	}
	// Service-linked role is filtered before GetRole; only young + old are enriched.
	if calls.Count("GetRole") != 2 {
		t.Errorf("expected 2 GetRole calls (service-linked skipped), got %d", calls.Count("GetRole"))
	}
}

func TestExecuteIAMRoles_DeleteTeardownOrder(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	calls := &awstest.Calls{}
	fakeCfg := awstest.Config(t, calls, awstest.Stubs{
		"ListRoles": &iam.ListRolesOutput{
			Roles: []iamtypes.Role{
				{RoleName: aws.String("old"), Path: aws.String("/"), Arn: aws.String("arn:aws:iam::111:role/old")},
			},
		},
		"GetRole": &iam.GetRoleOutput{Role: &iamtypes.Role{
			RoleName:   aws.String("old"),
			Arn:        aws.String("arn:aws:iam::111:role/old"),
			Path:       aws.String("/"),
			CreateDate: aws.Time(time.Now().Add(-200 * 24 * time.Hour)),
		}},
		"ListAttachedRolePolicies": &iam.ListAttachedRolePoliciesOutput{
			AttachedPolicies: []iamtypes.AttachedPolicy{
				{PolicyName: aws.String("p1"), PolicyArn: aws.String("arn:aws:iam::111:policy/p1")},
			},
		},
		"ListRolePolicies": &iam.ListRolePoliciesOutput{
			PolicyNames: []string{"inline1"},
		},
		"ListInstanceProfilesForRole": &iam.ListInstanceProfilesForRoleOutput{
			InstanceProfiles: []iamtypes.InstanceProfile{
				{InstanceProfileName: aws.String("prof1")},
			},
		},
		"DetachRolePolicy":              &iam.DetachRolePolicyOutput{},
		"DeleteRolePolicy":              &iam.DeleteRolePolicyOutput{},
		"RemoveRoleFromInstanceProfile": &iam.RemoveRoleFromInstanceProfileOutput{},
		"DeleteRole":                    &iam.DeleteRoleOutput{},
	})
	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{IAM: iam.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"iamroles", "--region", "us-east-1", "--delete", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}

	for _, op := range []string{"DetachRolePolicy", "DeleteRolePolicy", "RemoveRoleFromInstanceProfile", "DeleteRole"} {
		if calls.Count(op) != 1 {
			t.Errorf("expected 1 %s call, got %d", op, calls.Count(op))
		}
	}
	names := calls.Names()
	detach := firstIndex(names, "DetachRolePolicy")
	inline := firstIndex(names, "DeleteRolePolicy")
	profile := firstIndex(names, "RemoveRoleFromInstanceProfile")
	del := firstIndex(names, "DeleteRole")
	if detach >= inline || inline >= profile || profile >= del {
		t.Errorf("teardown order violated: detach=%d inline=%d profile=%d deleteRole=%d\ncalls: %v", detach, inline, profile, del, names)
	}
}

func TestExecuteIAMRoles_RequireTagGate(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	prevCfg, prevBuild := newConfigFn, buildClientsFn
	t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

	fakeCfg := awstest.Config(t, nil, awstest.Stubs{
		"ListRoles": &iam.ListRolesOutput{
			Roles: []iamtypes.Role{
				{RoleName: aws.String("alpha"), Path: aws.String("/"), Arn: aws.String("arn:aws:iam::111:role/alpha")},
				{RoleName: aws.String("beta"), Path: aws.String("/"), Arn: aws.String("arn:aws:iam::111:role/beta")},
			},
		},
		"GetRole": func(_ context.Context, in any) (any, error) {
			gi := in.(*iam.GetRoleInput)
			role := &iamtypes.Role{
				RoleName:   gi.RoleName,
				Arn:        aws.String("arn:aws:iam::111:role/" + aws.ToString(gi.RoleName)),
				Path:       aws.String("/"),
				CreateDate: aws.Time(time.Now().Add(-200 * 24 * time.Hour)),
			}
			if aws.ToString(gi.RoleName) == "alpha" {
				role.Tags = []iamtypes.Tag{{Key: aws.String("cor-managed"), Value: aws.String("true")}}
			}
			return &iam.GetRoleOutput{Role: role}, nil
		},
		"ListAttachedRolePolicies":    &iam.ListAttachedRolePoliciesOutput{},
		"ListRolePolicies":            &iam.ListRolePoliciesOutput{},
		"ListInstanceProfilesForRole": &iam.ListInstanceProfilesForRoleOutput{},
	})
	newConfigFn = func(_ context.Context, _ handlers.CloudConfig, _ string, _ bool, _ bool) (*aws.Config, error) {
		return &fakeCfg, nil
	}
	buildClientsFn = func(_ CommandSetup, _ *aws.Config) *handlers.AWSClientImpl {
		return &handlers.AWSClientImpl{IAM: iam.NewFromConfig(fakeCfg)}
	}

	out := &bytes.Buffer{}
	cmd := rootCmdForTest(out, []string{"iamroles", "--region", "us-east-1", "--require-tag", "cor-managed"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "alpha") {
		t.Errorf("expected alpha (matching tag) to surface, got:\n%s", got)
	}
	if strings.Contains(got, "beta") {
		t.Errorf("beta lacks the required tag and must be gated out, got:\n%s", got)
	}
}
