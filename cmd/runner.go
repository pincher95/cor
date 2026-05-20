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
	"os"

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

// ceConsumers names the subcommands whose MonthlyCost callbacks actually
// consult CEActuals. Other commands skip the (paid) CE round-trip even
// when --with-ce is set.
var ceConsumers = map[string]bool{
	"snapshots": true,
	"images":    true,
	"rds":       true,
}

// globalServices names subcommands whose AWS API is account-global —
// listing returns the same resources regardless of which region the call
// is made from. With --all-regions we run them once instead of N times to
// avoid N× duplicate rows.
var globalServices = map[string]bool{
	"s3buckets":    true,
	"route53zones": true,
}

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

	if globals.WithCE && ceConsumers[cmd.Name()] {
		act, ceErr := cost.FetchCEActuals(ctx, *cfg)
		if ceErr != nil {
			awsCmd.Logger.LogError("--with-ce: failed to fetch Cost Explorer actuals; falling back to estimates", ceErr, nil)
		} else {
			awsCmd.CEActuals = act
		}
	}

	if !globals.AllRegions || globalServices[cmd.Name()] {
		return execute(awsCmd, ctx, globals, extras)
	}

	return runAcrossRegions(ctx, awsCmd, cmd, setup, globals, extras, execute)
}

// runAcrossRegions fans the same command out across every enabled region for
// the account. Each region runs with its own aws.Config + clients + Pricing;
// regions execute concurrently under a small parallel cap to avoid
// overwhelming any single API. With --delete, each region's pipeline prompts
// independently via confirmDelete unless --yes is also set.
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
	if globals.Delete {
		mode := "per-region confirmation prompts"
		if globals.AssumeYes {
			mode = "NO PROMPTS (--yes)"
		}
		awsCmd.Logger.LogInfo("multi-region delete enabled", map[string]any{
			"regions": len(regions),
			"mode":    mode,
		})
	}

	parallelism := 5
	if globals.Delete && !globals.AssumeYes {
		// Per-region prompts can't be interleaved sensibly; serialize when
		// the user expects to confirm interactively.
		parallelism = 1
	}

	// One sink across all regions — rows from every region land in a single
	// unified table. Lazily constructed on the first per-region write so the
	// header reflects the actual decorated columns (Region + cost). After
	// all regions complete, we emit one grand-total footer before Close.
	shared := NewSharedSink(globals.Format, cmd.OutOrStdout())
	if globals.SortBy != "" {
		shared.SetSort(resolveSortKey(globals.SortBy), globals.SortDesc)
	}
	defer func() {
		shared.FinalizeTotals()
		shared.Close()
	}()

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallelism)
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
			regional.SharedSink = shared
			regional.CEActuals = awsCmd.CEActuals
			if err := execute(regional, gctx, globals, extras); err != nil {
				awsCmd.Logger.LogError("multi-region: command failed", err, map[string]any{"region": region})
			}
			return nil
		})
	}
	return g.Wait()
}

// newAWSCommandWithFormat builds an AWSCommand with the given log format,
// stdin/stdout, and pre-built service clients. Logs go to stderr so they
// don't interleave the table on stdout — this lets users redirect the
// table cleanly (`cor … > out.txt`) while still seeing progress.
func newAWSCommandWithFormat(client *handlers.AWSClientImpl, cloudConfig *handlers.CloudConfig, in io.Reader, out io.Writer, logFormat string) *AWSCommand {
	return &AWSCommand{
		AWSClient:   *client,
		CloudConfig: cloudConfig,
		Logger:      logging.NewLoggerWithFormat(logFormat, os.Stderr),
		Prompter:    prompter.NewConsolePrompter(in, out),
		Output:      out,
		Pricing:     cost.New(aws.ToString(cloudConfig.Region)),
	}
}
