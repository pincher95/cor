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
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// CommandSetup declares per-command variation for runResourceCommand.
// AdditionalFlags lists flags specific to this subcommand (global flags are
// always included). BuildClients constructs the minimal AWSClientImpl the
// command needs from the resolved aws.Config; it must not return nil.
type CommandSetup struct {
	AdditionalFlags []flags.Flag
	BuildClients    func(cfg *aws.Config) *handlers.AWSClientImpl
}

// newConfigFn is a test seam: tests override it to avoid hitting real AWS config resolution.
var newConfigFn = handlers.NewConfig

// buildClientsFn is a test seam: tests override it to inject fake clients
// without changing per-command BuildClients callbacks. Production calls
// setup.BuildClients(cfg) unchanged.
var buildClientsFn = func(setup CommandSetup, cfg *aws.Config) *handlers.AWSClientImpl {
	return setup.BuildClients(cfg)
}

// runResourceCommand handles the boilerplate phase of every resource command:
// flag retrieval, AWS config resolution, client assembly, and AWSCommand packaging.
// The execute callback receives the fully assembled AWSCommand plus the typed
// globals and the per-command extras map, and performs the command-specific
// work. Bind existing methods via Go's method-expression form, e.g.
// (*AWSCommand).executeVolumes.
//
// The execute callback's argument order — (awsCmd, ctx, globals, extras) — is
// load-bearing: do not reorder. The *AWSCommand-first position is required by
// the method-expression form so bindings like (*AWSCommand).executeVolumes
// match the parameter type without any change to the 25 existing executeX
// methods.
func runResourceCommand(
	cmd *cobra.Command,
	setup CommandSetup,
	execute func(awsCmd *AWSCommand, ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error,
) error {
	ctx := cmd.Context()

	flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
	globals, extras, err := flags.GetFlags(flagRetriever, setup.AdditionalFlags)
	if err != nil {
		return err
	}

	cloudConfig := &handlers.CloudConfig{
		AuthMethod: aws.String(globals.AuthMethod),
		Profile:    aws.String(globals.Profile),
		Region:     aws.String(globals.Region),
	}
	cfg, err := newConfigFn(ctx, *cloudConfig, "UTC", true, true)
	if err != nil {
		return err
	}

	client := buildClientsFn(setup, cfg)
	if client == nil {
		return fmt.Errorf("runResourceCommand: BuildClients returned nil")
	}
	awsCmd := newAWSCommandWithFormat(client, cloudConfig, cmd.InOrStdin(), cmd.OutOrStdout(), globals.LogFormat)

	if !globals.AllRegions {
		return execute(awsCmd, ctx, globals, extras)
	}

	return runAcrossRegions(ctx, awsCmd, cmd, setup, globals, extras, execute)
}

// runAcrossRegions fans the same command out across every enabled region for
// the account. Each region runs with its own aws.Config + clients + Pricing;
// regions execute concurrently under a small parallel cap to avoid
// overwhelming any single API. --delete is disabled in multi-region mode
// unless --yes is also set (extra blast-radius guard).
func runAcrossRegions(
	ctx context.Context,
	awsCmd *AWSCommand,
	cmd *cobra.Command,
	setup CommandSetup,
	globals *flags.GlobalFlags,
	extras *map[string]any,
	execute func(awsCmd *AWSCommand, ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error,
) error {
	ec2Client := awsCmd.AWSClient.EC2
	if ec2Client == nil {
		// EC2 isn't part of this command's BuildClients; build a transient
		// one just for region discovery without mutating the caller's struct.
		regionCfg, err := newConfigFn(ctx, *awsCmd.CloudConfig, "UTC", true, true)
		if err != nil {
			return fmt.Errorf("multi-region: failed to resolve config for region discovery: %w", err)
		}
		ec2Client = ec2.NewFromConfig(*regionCfg)
	}
	regions, err := enabledRegions(ctx, ec2Client)
	if err != nil {
		return fmt.Errorf("multi-region: failed to list regions: %w", err)
	}
	if globals.Delete && !globals.AssumeYes {
		return fmt.Errorf("--all-regions with --delete requires --yes (multi-region blast radius)")
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(5)
	for _, region := range regions {
		g.Go(func() error {
			regionalCloudCfg := &handlers.CloudConfig{
				AuthMethod: awsCmd.CloudConfig.AuthMethod,
				Profile:    awsCmd.CloudConfig.Profile,
				Region:     aws.String(region),
			}
			cfg, err := newConfigFn(gctx, *regionalCloudCfg, "UTC", true, true)
			if err != nil {
				awsCmd.Logger.LogError("multi-region: config failed", err, map[string]any{"region": region})
				return nil
			}
			client := buildClientsFn(setup, cfg)
			if client == nil {
				return nil
			}
			regional := newAWSCommandWithFormat(client, regionalCloudCfg, cmd.InOrStdin(), cmd.OutOrStdout(), globals.LogFormat)
			if err := execute(regional, gctx, globals, extras); err != nil {
				awsCmd.Logger.LogError("multi-region: command failed", err, map[string]any{"region": region})
			}
			return nil
		})
	}
	return g.Wait()
}

// newAWSCommandWithFormat builds an AWSCommand with the given log format,
// stdin/stdout, and pre-built service clients.
func newAWSCommandWithFormat(client *handlers.AWSClientImpl, cloudConfig *handlers.CloudConfig, in io.Reader, out io.Writer, logFormat string) *AWSCommand {
	return &AWSCommand{
		AWSClient:   *client,
		CloudConfig: cloudConfig,
		Logger:      logging.NewLoggerWithFormat(logFormat, out),
		Prompter:    prompter.NewConsolePrompter(in, out),
		Output:      out,
		Pricing:     cost.New(aws.ToString(cloudConfig.Region)),
	}
}
