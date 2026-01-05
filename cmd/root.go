/*
Copyright 2024 Elastic Scaler Contributors.

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
	"time"

	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	NumGoroutines = 10
)

type AWSCommand struct {
	AWSClient handlers.AWSClientImpl
	Logger    *logging.Logger
	Prompter  prompter.Client
	Output    io.Writer
}

var cfgFile string

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "cor",
	Short: "Delete orphaned AWS resources",
	Long: `
	A command line tool to delete orphaned AWS resources.`,
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
	rootCmd.PersistentFlags().StringP("auth-method", "a", "AWS_CREDENTIALS_FILE", "AWS authentication methos AWS_CREDENTIALS_FILE/IAM_ARN/ENV_SECRET")
	rootCmd.PersistentFlags().Bool("delete", false, "Delete Orphant resources")
	rootCmd.PersistentFlags().String("sort-by", "", "Sort output by column name (buffers results in memory; disables streaming)")
	rootCmd.PersistentFlags().Bool("sort-desc", false, "Sort output in descending order")
	rootCmd.PersistentFlags().Duration("timeout", 0, "Timeout in seconds for the command execution.")

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

	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		fmt.Fprintln(os.Stderr, "Using config file:", viper.ConfigFileUsed())
	}
}

// GetRootCommand returns the root command
func GetRootCommand() *cobra.Command {
	return rootCmd
}
