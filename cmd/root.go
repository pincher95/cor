/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pincher95/cor/pkg/cost"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	NumGoroutines = 10
)

type AWSCommand struct {
	AWSClient   handlers.AWSClientImpl
	CloudConfig *handlers.CloudConfig
	Logger      *logging.Logger
	Prompter    prompter.Client
	Output      io.Writer
	Pricing     *cost.Pricing
	// SharedSink, when non-nil, causes the pipeline to write rows into a
	// single cross-region table instead of building its own per-region sink.
	// Set by runAcrossRegions; nil for single-region runs.
	SharedSink *SharedSink
}

// SharedSink is a lazy-once container for a thread-safe RowSink used by
// --all-regions to unify per-region output into a single table.
type SharedSink struct {
	once   sync.Once
	sink   printer.RowSink
	format string
	out    io.Writer
}

// NewSharedSink constructs an empty SharedSink. The first pipeline call
// builds the underlying sink with its headers; subsequent calls reuse it.
func NewSharedSink(format string, out io.Writer) *SharedSink {
	return &SharedSink{format: format, out: out}
}

// Get returns (constructing on first call) the wrapped sink. Concurrent
// callers share one sink; writes are mutex-serialized.
func (s *SharedSink) Get(index bool, headers []string) printer.RowSink {
	s.once.Do(func() {
		s.sink = printer.NewConcurrentSink(printer.NewSink(s.format, s.out, index, headers))
	})
	return s.sink
}

// Close finalizes the wrapped sink. Safe to call when no pipeline has written
// to it yet (the sink is just nil in that case).
func (s *SharedSink) Close() {
	if s.sink != nil {
		s.sink.Close()
	}
}

var cfgFile string

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "cor",
	Short: "Delete orphaned AWS resources",
	Long: `
	A command line tool to delete orphaned AWS resources.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		return applyTimeout(cmd)
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		cancelTimeout(cmd)
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute(ctx context.Context) error {
	logger := logging.NewLogger()
	start := time.Now()
	err := rootCmd.ExecuteContext(ctx)
	logger.LogInfo("Time taken to process:", map[string]any{"time": time.Since(start).String()})
	if err != nil {
		return err
	}
	return nil
}

func addSubcommandsPallets() {
	rootCmd.AddCommand(volumesCmd)
	rootCmd.AddCommand(snapshotsCmd)
	rootCmd.AddCommand(imagesCmd)
	rootCmd.AddCommand(elasticIPsCmd)
	rootCmd.AddCommand(enisCmd)
	rootCmd.AddCommand(targetgroupsCmd)
	rootCmd.AddCommand(elbv1Cmd)
	rootCmd.AddCommand(elbv2Cmd)
	rootCmd.AddCommand(autoscalingCmd)
	rootCmd.AddCommand(natgatewaysCmd)
	rootCmd.AddCommand(rdsCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(efsCmd)
	rootCmd.AddCommand(ecrCmd)
	rootCmd.AddCommand(route53ZonesCmd)
	rootCmd.AddCommand(vpcEndpointsCmd)
	rootCmd.AddCommand(clientVPNCmd)
	rootCmd.AddCommand(vpnConnectionsCmd)
	rootCmd.AddCommand(tgwAttachmentsCmd)
	rootCmd.AddCommand(lambdaCmd)
	rootCmd.AddCommand(elasticacheCmd)
	rootCmd.AddCommand(opensearchCmd)
	rootCmd.AddCommand(dynamodbCmd)
	rootCmd.AddCommand(s3bucketsCmd)
	rootCmd.AddCommand(ecsCmd)
}

func init() {
	cobra.OnInitialize(initConfig)

	// Here you will define your flags and configuration settings.
	addSubcommandsPallets()
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.cor.yaml)")
	rootCmd.PersistentFlags().StringP("region", "r", "us-east-1", "AWS region")
	rootCmd.PersistentFlags().StringP("profile", "p", "default", "AWS credentials file profile")
	rootCmd.PersistentFlags().StringP("auth-method", "a", "AWS_CREDENTIALS_FILE", "AWS authentication method: AWS_CREDENTIALS_FILE or ENV_SECRET")
	rootCmd.PersistentFlags().Bool("delete", false, "Delete orphaned resources")
	rootCmd.PersistentFlags().String("sort-by", "", "Sort output by column name (buffers results in memory; disables streaming)")
	rootCmd.PersistentFlags().Bool("sort-desc", false, "Sort output in descending order")
	rootCmd.PersistentFlags().Duration("timeout", 0, "Timeout in seconds for the command execution.")
	rootCmd.PersistentFlags().Bool("yes", false, "Skip the delete confirmation prompt. Use only in automation.")
	rootCmd.PersistentFlags().String("on-error", "stop", "Delete-phase error policy: stop (default) or continue.")
	rootCmd.PersistentFlags().String("log-format", "text", "Log output format: text (default) or json.")
	rootCmd.PersistentFlags().String("metrics-file", "", "Write per-run JSON metrics summary to this path on completion.")
	rootCmd.PersistentFlags().Bool("dry-run", false, "Print what would be deleted without calling AWS delete APIs.")
	rootCmd.PersistentFlags().String("format", "table", "Output format: table (default), json, or csv.")
	rootCmd.PersistentFlags().String("state-file", "", "Path to a persistent state file. Already-deleted items recorded there are skipped on re-run.")
	rootCmd.PersistentFlags().Float64("min-cost", 0, "Only show orphans with estimated monthly cost >= this USD value.")
	rootCmd.PersistentFlags().Int("top-n", 0, "Show only the N most-expensive orphans (0 = all).")
	rootCmd.PersistentFlags().Bool("rank", false, "Add a Rank column ordering by estimated monthly cost desc.")
	rootCmd.PersistentFlags().Bool("all-regions", false, "Scan every enabled region for the current account.")
	rootCmd.PersistentFlags().String("save-baseline", "", "Write a JSON snapshot of this run's orphans+costs to this path.")
	rootCmd.PersistentFlags().String("diff-baseline", "", "Read a prior --save-baseline snapshot and emit a delta (added / removed / changed).")

	// imagesCmd.PersistentFlags().String("creation-date", "", "The time when the image was created, in the ISO 8601 format in the UTC time zone (YYYY-MM-DDThh:mm:ss.sssZ), for example, 2021-09-29T11:04:43.305Z . You can use a wildcard ( * ), for example, 2021-09-29T* , which matches an entire day")

	// NOTE: keep root flags minimal; subcommands provide the functional surface area.
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		// Find home directory.
		home, err := os.UserHomeDir()
		cobra.CheckErr(err)

		// Search config in home directory with name ".cor" (without extension).
		viper.AddConfigPath(home)
		viper.SetConfigType("yaml")
		viper.SetConfigName(".cor")
	}

	// Support config/env overrides for flags (hyphens -> underscores) with a stable prefix.
	// Example: COR_REGION, COR_AUTH_METHOD, COR_SORT_BY
	viper.SetEnvPrefix("COR")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		fmt.Fprintln(os.Stderr, "Using config file:", viper.ConfigFileUsed())
	}
}

type timeoutCancelKey struct{}

func applyTimeout(cmd *cobra.Command) error {
	timeout, err := getDurationFlag(cmd, "timeout")
	if err != nil {
		return err
	}
	if timeout <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	cmd.SetContext(context.WithValue(ctx, timeoutCancelKey{}, cancel))
	return nil
}

func cancelTimeout(cmd *cobra.Command) {
	if cancel, ok := cmd.Context().Value(timeoutCancelKey{}).(context.CancelFunc); ok {
		cancel()
	}
}

func getDurationFlag(cmd *cobra.Command, name string) (time.Duration, error) {
	if cmd.Flags().Lookup(name) != nil {
		return cmd.Flags().GetDuration(name)
	}
	if cmd.InheritedFlags().Lookup(name) != nil {
		return cmd.InheritedFlags().GetDuration(name)
	}
	return cmd.PersistentFlags().GetDuration(name)
}

// GetRootCommand returns the root command
func GetRootCommand() *cobra.Command {
	return rootCmd
}
