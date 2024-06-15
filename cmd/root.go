/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"fmt"
	"os"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cfgFile string
var tableRows []table.Row

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "cor",
	Short: "A brief description of your application",
	Long: `A longer description that spans multiple lines and likely contains
examples and usage of using your application. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
	// Uncomment the following line if your bare application
	// has an action associated with it:
	// Run: func(cmd *cobra.Command, args []string) { },
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func addSubcommandsPallets() {
	rootCmd.AddCommand(volumesCmd)
	rootCmd.AddCommand(snapshotsCmd)
	rootCmd.AddCommand(imagesCmd)
	rootCmd.AddCommand(elasticaddressesCmd)
	rootCmd.AddCommand(elbv1Cmd)
	rootCmd.AddCommand(elbv2Cmd)
}

func init() {
	cobra.OnInitialize(initConfig)

	// Here you will define your flags and configuration settings.
	addSubcommandsPallets()
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.cor.yaml)")
	rootCmd.PersistentFlags().StringP("region", "r", "us-east-1", "AWS region")
	rootCmd.PersistentFlags().StringP("profile", "p", "default", "AWS credentiol file profile")
	rootCmd.PersistentFlags().StringP("auth-method", "a", "AWS_CREDENTIALS_FILE", "AWS authentication methos AWS_CREDENTIALS_FILE/IAM_ARN/ENV_SECRET")
	rootCmd.PersistentFlags().Bool("delete", false, "Delete Orphant resources")

	// imagesCmd.PersistentFlags().String("creation-date", "", "The time when the image was created, in the ISO 8601 format in the UTC time zone (YYYY-MM-DDThh:mm:ss.sssZ), for example, 2021-09-29T11:04:43.305Z . You can use a wildcard ( * ), for example, 2021-09-29T* , which matches an entire day")

	// Cobra also supports local flags, which will only run
	// when this action is called directly.
	rootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
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
