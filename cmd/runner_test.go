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
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestRunResourceCommand_HappyPath(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	originalNewConfigFn := newConfigFn
	t.Cleanup(func() { newConfigFn = originalNewConfigFn })

	var newConfigCalled bool
	newConfigFn = func(ctx context.Context, cc handlers.CloudConfig, tz string, humanize, debug bool) (*aws.Config, error) {
		newConfigCalled = true
		if aws.ToString(cc.Region) != "us-west-2" {
			t.Errorf("expected region us-west-2, got %q", aws.ToString(cc.Region))
		}
		if aws.ToString(cc.Profile) != "default" {
			t.Errorf("expected profile default, got %q", aws.ToString(cc.Profile))
		}
		return &aws.Config{Region: aws.ToString(cc.Region)}, nil
	}

	cmd := &cobra.Command{Use: "test"}
	cmd.PersistentFlags().String("region", "us-west-2", "")
	cmd.PersistentFlags().String("profile", "default", "")
	cmd.PersistentFlags().String("auth-method", "AWS_CREDENTIALS_FILE", "")
	cmd.PersistentFlags().Bool("delete", false, "")
	cmd.PersistentFlags().String("sort-by", "", "")
	cmd.PersistentFlags().Bool("sort-desc", false, "")
	cmd.PersistentFlags().Bool("yes", false, "")
	cmd.PersistentFlags().String("on-error", "stop", "")
	cmd.PersistentFlags().String("log-format", "text", "")
	cmd.PersistentFlags().String("metrics-file", "", "")
	cmd.PersistentFlags().Bool("dry-run", false, "")
	cmd.PersistentFlags().String("format", "table", "")
	cmd.PersistentFlags().String("state-file", "", "")
	cmd.PersistentFlags().Float64("min-cost", 0, "")
	cmd.PersistentFlags().Int("top-n", 0, "")
	cmd.PersistentFlags().Bool("rank", false, "")
	cmd.PersistentFlags().Bool("all-regions", false, "")
	cmd.PersistentFlags().String("save-baseline", "", "")
	cmd.PersistentFlags().String("diff-baseline", "", "")
	cmd.PersistentFlags().Bool("with-ce", false, "")
	cmd.Flags().String("filter-by-name", "", "")
	cmd.SetContext(context.Background())

	var (
		buildClientsCalled bool
		executeCalled      bool
		capturedAWSCmd     *AWSCommand
		capturedGlobals    *flags.GlobalFlags
		capturedExtras     *map[string]any
		capturedCfg        *aws.Config
	)

	setup := CommandSetup{
		AdditionalFlags: []flags.Flag{{Name: "filter-by-name", Type: "string"}},
		BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
			buildClientsCalled = true
			capturedCfg = cfg
			return &handlers.AWSClientImpl{}
		},
	}

	execute := func(awsCmd *AWSCommand, ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
		executeCalled = true
		capturedAWSCmd = awsCmd
		capturedGlobals = globals
		capturedExtras = extras
		return nil
	}

	if err := runResourceCommand(cmd, setup, execute); err != nil {
		t.Fatalf("runResourceCommand returned error: %v", err)
	}

	if !newConfigCalled {
		t.Error("expected newConfigFn to be called")
	}
	if !buildClientsCalled {
		t.Error("expected BuildClients to be called")
	}
	if capturedCfg == nil {
		t.Error("expected BuildClients to receive non-nil config")
	}
	if !executeCalled {
		t.Fatal("expected execute to be called")
	}
	if capturedAWSCmd == nil {
		t.Fatal("expected execute to receive non-nil AWSCommand")
	}
	if capturedAWSCmd.Logger == nil {
		t.Error("expected AWSCommand.Logger to be set")
	}
	if capturedAWSCmd.Prompter == nil {
		t.Error("expected AWSCommand.Prompter to be set")
	}
	if capturedAWSCmd.Output == nil {
		t.Error("expected AWSCommand.Output to be set")
	}
	if capturedAWSCmd.CloudConfig == nil {
		t.Error("expected AWSCommand.CloudConfig to be set")
	}

	if capturedGlobals == nil {
		t.Fatal("expected execute to receive non-nil globals")
	}
	if capturedGlobals.Region != "us-west-2" {
		t.Errorf("expected globals.Region to be us-west-2, got %q", capturedGlobals.Region)
	}
	if capturedGlobals.Profile != "default" {
		t.Errorf("expected globals.Profile to be default, got %q", capturedGlobals.Profile)
	}
	if capturedGlobals.AuthMethod != "AWS_CREDENTIALS_FILE" {
		t.Errorf("expected globals.AuthMethod to be AWS_CREDENTIALS_FILE, got %q", capturedGlobals.AuthMethod)
	}
	if capturedGlobals.Delete != false {
		t.Errorf("expected globals.Delete to be false, got %v", capturedGlobals.Delete)
	}
	if capturedGlobals.SortBy != "" {
		t.Errorf("expected globals.SortBy to be empty, got %q", capturedGlobals.SortBy)
	}
	if capturedGlobals.SortDesc != false {
		t.Errorf("expected globals.SortDesc to be false, got %v", capturedGlobals.SortDesc)
	}
	if capturedExtras == nil {
		t.Fatal("expected execute to receive non-nil extras")
	}
	if _, ok := (*capturedExtras)["filter-by-name"]; !ok {
		t.Errorf("expected extras to contain 'filter-by-name'")
	}
	for _, globalKey := range []string{"region", "profile", "auth-method", "delete", "sort-by", "sort-desc", "yes", "on-error", "log-format", "metrics-file", "dry-run", "format", "state-file", "min-cost", "top-n", "rank", "all-regions", "save-baseline", "diff-baseline", "with-ce"} {
		if _, ok := (*capturedExtras)[globalKey]; ok {
			t.Errorf("extras should not contain global key %q", globalKey)
		}
	}
}
