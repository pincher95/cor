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
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
)

// CommandSetup declares per-command variation for runResourceCommand.
// AdditionalFlags lists flags specific to this subcommand (global flags are
// always included). BuildClients constructs the minimal AWSClientImpl the
// command needs from the resolved aws.Config.
type CommandSetup struct {
	AdditionalFlags []flags.Flag
	BuildClients    func(cfg *aws.Config) *handlers.AWSClientImpl
}

// newConfigFn is a test seam: tests override it to avoid hitting real AWS config resolution.
var newConfigFn = handlers.NewConfig

// runResourceCommand handles the boilerplate phase of every resource command:
// flag retrieval, AWS config resolution, client assembly, and AWSCommand packaging.
// The execute callback receives the fully assembled AWSCommand plus the flag values
// and performs the command-specific work. Bind existing methods via Go's
// method-expression form, e.g. (*AWSCommand).executeVolumes.
func runResourceCommand(
	cmd *cobra.Command,
	setup CommandSetup,
	execute func(awsCmd *AWSCommand, ctx context.Context, flagValues *map[string]any) error,
) error {
	ctx := cmd.Context()

	flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
	flagValues, err := flags.GetFlags(flagRetriever, setup.AdditionalFlags)
	if err != nil {
		return err
	}

	cloudConfig := &handlers.CloudConfig{
		AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
		Profile:    aws.String((*flagValues)["profile"].(string)),
		Region:     aws.String((*flagValues)["region"].(string)),
	}
	cfg, err := newConfigFn(ctx, *cloudConfig, "UTC", true, true)
	if err != nil {
		return err
	}

	awsCmd := newAWSCommand(setup.BuildClients(cfg), cloudConfig, os.Stdin, os.Stdout)
	return execute(awsCmd, ctx, flagValues)
}

// newAWSCommand assembles an AWSCommand with the default logger/prompter/output
// wiring. Split out for readability and testability.
func newAWSCommand(client *handlers.AWSClientImpl, cloudConfig *handlers.CloudConfig, in io.Reader, out io.Writer) *AWSCommand {
	return &AWSCommand{
		AWSClient:   *client,
		CloudConfig: cloudConfig,
		Logger:      logging.NewLogger(),
		Prompter:    prompter.NewConsolePrompter(in, out),
		Output:      out,
	}
}
