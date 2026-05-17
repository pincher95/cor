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

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// rootCmdForTest configures the package-global rootCmd to write to `out` and
// run with the given args. Resets all flag values to their defaults so prior
// test state does not leak.
func rootCmdForTest(out io.Writer, args []string) *cobra.Command {
	resetFlags(rootCmd)
	rootCmd.SetOut(out)
	rootCmd.SetErr(out)
	rootCmd.SetArgs(args)
	rootCmd.SetContext(context.Background())
	return rootCmd
}

func resetFlags(cmd *cobra.Command) {
	resetOne := func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	}
	cmd.Flags().VisitAll(resetOne)
	cmd.PersistentFlags().VisitAll(resetOne)
	for _, sub := range cmd.Commands() {
		resetFlags(sub)
	}
}
