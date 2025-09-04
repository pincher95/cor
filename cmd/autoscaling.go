package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
)

// autoscalingCmd represents the autoscaling command
var autoscalingCmd = &cobra.Command{
	Use:   "autoscaling",
	Short: "Delete orphaned AWS Auto Scaling Groups",
	Long:  `Find and optionally delete unused Auto Scaling Groups (ASG) that have no active instances attached`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Prepare helpers
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout
		ctx := cmd.Context()

		// Retrieve global + command specific flags
		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		flagValues, err := flags.GetFlags(flagRetriever, nil) // no extra flags yet
		if err != nil {
			return err
		}

		// Create shared AWS config based on flags
		cloudCfg := &handlers.CloudConfig{
			AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
			Profile:    aws.String((*flagValues)["profile"].(string)),
			Region:     aws.String((*flagValues)["region"].(string)),
		}

		cfg, err := handlers.NewConfig(ctx, *cloudCfg, "UTC", true, true)
		if err != nil {
			return err
		}

		// Create service clients
		asgClient := autoscaling.NewFromConfig(*cfg)
		stsClient := sts.NewFromConfig(*cfg)

		awsClient := &handlers.AWSClientImpl{
			ASG: asgClient,
			STS: stsClient,
		}

		return runAutoscalingCmd(ctx, prompterClient, output, awsClient, flagValues)
	},
}

func runAutoscalingCmd(ctx context.Context, prompter prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  prompter,
		Output:    output,
	}

	return command.executeAutoscaling(ctx, flagValues)
}

func (b *AWSCommand) executeAutoscaling(ctx context.Context, flagValues *map[string]any) error {
	// Placeholder: simply list Auto Scaling groups count for now
	groups, err := b.AWSClient.ASG.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{})
	if err != nil {
		b.Logger.LogError("failed to describe autoscaling groups", err, nil, false)
		return err
	}

	if _, err := fmt.Fprintf(b.Output, "Found %d Auto Scaling groups (functionality not fully implemented yet)\n", len(groups.AutoScalingGroups)); err != nil {
		return err
	}

	return nil
}

func init() {
	// No command-specific flags yet – placeholder for future
}
