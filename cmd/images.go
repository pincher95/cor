/*
Copyright © 2024 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/spf13/cobra"
)

// imagesCmd represents the images command
var imagesCmd = &cobra.Command{
	Use:   "images",
	Short: "A brief description of your command",
	Long:  ``,
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.TODO()

		// Create a new logger and error handler
		logger := logging.NewLogger()

		// Get the flags from the command and also the additional flags specific to this command
		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		// Specify additional flags that are specific to this command
		additionalFlags := []flags.Flag{
			{
				Name: "filter-by-name",
				Type: "string",
			},
			{
				Name: "creation-date-before",
				Type: "string",
			},
			{
				Name: "creation-date-after",
				Type: "string",
			},
			{
				Name: "include-used-by-instance",
				Type: "bool",
			},
			{
				Name: "include-used-by-launch-template",
				Type: "bool",
			},
			{
				Name: "delete",
				Type: "bool",
			},
		}

		flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
		if err != nil {
			logger.LogError("Error getting flags", err, nil, true)
			return
		}

		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String(flagValues["auth-method"].(string)),
			Profile:    aws.String(flagValues["profile"].(string)),
			Region:     aws.String(flagValues["region"].(string)),
		}

		// Parse the creation date flags
		var beforeCreationDate, afterCreationDate *time.Time
		dateLayout := "2006-01-02"
		if flagValues["creation-date-before"].(string) != "" {
			t, err := time.Parse(dateLayout, flagValues["creation-date-before"].(string))
			if err != nil {
				logger.LogError("Error parsing creation date", err, nil, false)
				return
			}
			beforeCreationDate = &t
		}
		if flagValues["creation-date-after"].(string) != "" {
			t, err := time.Parse(dateLayout, flagValues["creation-date-after"].(string))
			if err != nil {
				logger.LogError("Error parsing creation date", err, nil, false)
				return
			}
			afterCreationDate = &t
		}

		// Create a new AWS client
		cfg, err := handlers.NewConfigV2(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			logger.LogError("Failed loading AWS client config", err, nil, true)
			return
		}

		// Create a new EC2 client
		ec2Client := ec2.NewFromConfig(*cfg)
		// Create a new STS client
		stsClient := sts.NewFromConfig(*cfg)
		// Create a new Autoscaling client
		// autoscalingClient := asg.NewFromConfig(*cfg)
		// Get the account ID
		ownerID, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			logger.LogError("Error getting caller identity", err, nil, false)
		}

		var wg sync.WaitGroup

		tableRows := make([]table.Row, 0)

		// Create a channels to process images concurrently
		imagesChan := make(chan ec2types.Image, 1000)
		errorChan := make(chan error, 1)
		doneChan := make(chan struct{})

		wg.Add(1)
		// Start a goroutine to fetch images and send them to the channel
		go func() {
			defer wg.Done()
			err := describeImages(ctx, ec2Client, imagesChan, ownerID.Account, aws.String(flagValues["filter-by-name"].(string)), beforeCreationDate, afterCreationDate)
			if err != nil {
				logger.LogError("Error describe images", err, nil, false)
			}
		}()

		// Wait for all goroutines to finish
		go func() {
			wg.Wait()
			close(doneChan)
		}()

		for {
			select {
			case err := <-errorChan:
				logger.LogError("Error during volume processing", err, nil, true)
				return
			case <-doneChan:
				for image := range imagesChan {
					snapshotIds := getSnapshotIds(image)

					var usedByInstances, usedByLaunchTemplates []string
					usedByInstances, err = describeInstances(ctx, ec2Client, image.ImageId)
					if err != nil {
						logger.LogError("Error describe instance", err, nil, false)
					}

					usedByLaunchTemplates, err = describeLaunchTemplate(ctx, ec2Client, image.ImageId)
					if err != nil {
						logger.LogError("Error describe launch template", err, nil, false)
					}

					if len(usedByInstances) == 0 && len(usedByLaunchTemplates) == 0 {
						tableRows = append(tableRows, table.Row{*image.Name, *image.ImageId, *image.CreationDate, strings.Join(snapshotIds, "\n"), "", ""})
						continue
					}

					if flagValues["include-used-by-instance"].(bool) || flagValues["include-used-by-launch-template"].(bool) {
						tableRows = append(tableRows, table.Row{*image.Name, *image.ImageId, *image.CreationDate, strings.Join(snapshotIds, "\n"), strings.Join(usedByInstances, "\n"), strings.Join(usedByLaunchTemplates, "\n")})
					}
				}
				columnConfig := []table.ColumnConfig{
					{
						Name:        "ami name",
						AlignHeader: text.AlignCenter,
					},
					{
						Name:        "ami id",
						AlignHeader: text.AlignCenter,
					},
					{
						Name:        "creation date",
						AlignHeader: text.AlignCenter,
					},
					{
						Name:        "snapshot ids",
						AlignHeader: text.AlignCenter,
					},
					{
						Name:        "used by Instance",
						AlignHeader: text.AlignCenter,
					},
					{
						Name:        "used by Launch Template",
						AlignHeader: text.AlignCenter,
					},
				}

				printerClient := printer.NewPrinter(os.Stdout, aws.Bool(true), &table.Row{"ami name", "ami id", "creation date", "snapshot ids", "used by Instance", "used by Launch Template"}, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}}, &columnConfig)

				if err := printerClient.PrintTextTable(&tableRows); err != nil {
					logger.LogError("Error printing table", err, nil, false)
				}

				if flagValues["delete"].(bool) && len(tableRows) > 0 {
					reader := bufio.NewReader(os.Stdin)
					logger.LogInfo("Are you sure you want to proceed? (yes/no): ", nil)
					response, _ := reader.ReadString('\n')
					response = strings.ToLower(strings.TrimSpace(response))

					if response == "yes" || response == "y" {
						for _, tableRow := range tableRows {
							_, err := ec2Client.DeregisterImage(ctx, &ec2.DeregisterImageInput{
								ImageId: aws.String(tableRow[1].(string)),
								// DryRun:  aws.Bool(!deleteImages),
								// DryRun: aws.Bool(true),
							})
							if err != nil {
								logger.LogError("Error deregistering image", err, nil, false)
							}

							// Verify that the image has been deregistered
							err = waitForImageDeregistration(context.TODO(), ec2Client, tableRow[1].(string))
							if err != nil {
								logger.LogError("Error verifying image deregistration", err, nil, false)
							}
							logger.LogInfo("Verified image has been deregistered", map[string]interface{}{"imageID": tableRow[1].(string)})

							for _, snapshot := range strings.Split(tableRow[3].(string), "\n") {
								_, err := ec2Client.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
									SnapshotId: aws.String(snapshot),
									// DryRun:     aws.Bool(!deleteImages),
									// DryRun: aws.Bool(true),
								})
								if err != nil {
									logger.LogError("Error deleting snapshot", err, nil, false)
								}
							}
						}

					} else if response == "no" || response == "n" {
						logger.LogInfo("Aborted.", nil)
					} else {
						logger.LogInfo("Invalid response. Please enter 'yes' or 'no'.", nil)
					}
				}
				return
			}
		}
	},
}

func init() {
	// rootCmd.AddCommand(imagesCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// imagesCmd.PersistentFlags().String("creation-date-before", time.Now().UTC().Format("2006-01-02"), "The time when the image was created, in the UTC time zone (YYYY-MM-DD), for example, 2021-09-29.")
	imagesCmd.PersistentFlags().String("creation-date-before", "", "The time when the image was created, in the UTC time zone (YYYY-MM-DD), for example, 2021-09-29.")

	imagesCmd.PersistentFlags().String("creation-date-after", "", "The time when the image was created, in the UTC time zone (YYYY-MM-DD), for example, 2021-09-29.")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// imagesCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
	imagesCmd.Flags().String("filter-by-name", "*", "The name of the AMI (provided during image creation) ,wildcard ( * ) can be used.")
	imagesCmd.Flags().Bool("include-used-by-instance", false, "Include images that are used by instances.")
	imagesCmd.Flags().Bool("include-used-by-launch-template", false, "Include images that are used by launch templates.")
}

func describeInstances(ctx context.Context, client *ec2.Client, instanceImageID *string) ([]string, error) {
	useedByIstances := make([]string, 0)
	paginator := ec2.NewDescribeInstancesPaginator(client, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("image-id"),
				Values: []string{*instanceImageID},
			},
		},
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, reservation := range page.Reservations {
			for _, instance := range reservation.Instances {
				useedByIstances = append(useedByIstances, *instance.InstanceId)
			}
		}
	}
	return useedByIstances, nil
}

func describeLaunchTemplate(ctx context.Context, client *ec2.Client, imageID *string) ([]string, error) {
	usedByLaunchTemplates := make([]string, 0)
	paginatorLaunchTemplate := ec2.NewDescribeLaunchTemplatesPaginator(client, &ec2.DescribeLaunchTemplatesInput{
		Filters: []ec2types.Filter{
			// {
			// 	Name:   aws.String("launch-template-id"),
			// 	Values: []string{*launchTemplateID},
			// },
		},
	})
	for paginatorLaunchTemplate.HasMorePages() {
		page, err := paginatorLaunchTemplate.NextPage(ctx)
		if err != nil {

			return nil, err
		}

		for _, launchTemplate := range page.LaunchTemplates {
			// Check if the launch template has the tag we want to exclude
			shouldExclude := true
			for _, tag := range launchTemplate.Tags {
				tagKey := aws.ToString(tag.Key)
				if strings.Contains(tagKey, "karpenter") {
					shouldExclude = false
				}
			}

			if shouldExclude {
				// Get the default version of the launch template and check if it uses the image
				paginatorLaunchTemplateVersion := ec2.NewDescribeLaunchTemplateVersionsPaginator(client, &ec2.DescribeLaunchTemplateVersionsInput{
					LaunchTemplateId: launchTemplate.LaunchTemplateId,
					Versions:         []string{"$Default"},
				})
				for paginatorLaunchTemplateVersion.HasMorePages() {
					page, err := paginatorLaunchTemplateVersion.NextPage(ctx)
					if err != nil {
						return nil, err
					}

					for _, version := range page.LaunchTemplateVersions {
						amiID := aws.ToString(version.LaunchTemplateData.ImageId)
						targetAMI := aws.ToString(imageID)
						isDefault := aws.ToBool(version.DefaultVersion)

						// Ensure pointers are not nil before dereferencing
						if version.LaunchTemplateData != nil && version.LaunchTemplateData.ImageId != nil && imageID != nil && version.DefaultVersion != nil && *version.DefaultVersion {
							if targetAMI == amiID && isDefault {
								usedByLaunchTemplates = append(usedByLaunchTemplates, *launchTemplate.LaunchTemplateId)
							}
						}
					}
				}
			}
		}
	}

	return usedByLaunchTemplates, nil
}

func describeImages(ctx context.Context, client *ec2.Client, imagesChan chan<- ec2types.Image, ownerID, imageName *string, beforeCreationDate, afterCreationDate *time.Time) error {
	paginator := ec2.NewDescribeImagesPaginator(client, &ec2.DescribeImagesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("owner-id"),
				Values: []string{*ownerID},
			},
			{
				Name:   aws.String("name"),
				Values: []string{*imageName},
			},
		},
		Owners: []string{*ownerID, "self"},
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			close(imagesChan)
			return err
		}

		for _, image := range page.Images {
			imageCreationDate, err := time.Parse(time.RFC3339, *image.CreationDate)
			if err != nil {
				close(imagesChan)
				return err
			}

			if beforeCreationDate == nil && afterCreationDate == nil {
				imagesChan <- image
				continue
			}

			if beforeCreationDate != nil && afterCreationDate != nil && beforeCreationDate.Equal(*afterCreationDate) {
				if imageCreationDate.Equal(*beforeCreationDate) && imageCreationDate.Equal(*afterCreationDate) {
					imagesChan <- image
					continue
				}
			}

			if beforeCreationDate != nil && afterCreationDate == nil {
				if imageCreationDate.Before(*beforeCreationDate) {
					imagesChan <- image
				}
				continue
			}

			if beforeCreationDate == nil && afterCreationDate != nil {
				if imageCreationDate.After(*afterCreationDate) {
					imagesChan <- image
				}
				continue
			}

			if beforeCreationDate != nil && afterCreationDate != nil {
				if beforeCreationDate.After(*afterCreationDate) {
					if imageCreationDate.Before(imageCreationDate) && imageCreationDate.After(*afterCreationDate) {
						imagesChan <- image
					}
				} else if beforeCreationDate.Before(*afterCreationDate) {
					if imageCreationDate.Before(*beforeCreationDate) || imageCreationDate.After(*afterCreationDate) {
						imagesChan <- image
					}
				}
				continue
			}
		}
	}

	close(imagesChan)
	return nil
}

func getSnapshotIds(image ec2types.Image) []string {
	snapshotIds := make([]string, 0, len(image.BlockDeviceMappings))
	for _, mapping := range image.BlockDeviceMappings {
		if mapping.Ebs != nil {
			snapshotIds = append(snapshotIds, *mapping.Ebs.SnapshotId)
		}
	}
	return snapshotIds
}

// func getInstances(instanceChan <-chan ec2types.Instance, imageID *string) []string {
// 	usedByInstances := make([]string, 0)
// 	for instance := range instanceChan {
// 		if *instance.ImageId == *imageID {
// 			usedByInstances = append(usedByInstances, *instance.InstanceId)
// 		}
// 	}
// 	return usedByInstances
// }

func waitForImageDeregistration(ctx context.Context, client *ec2.Client, imageID string) error {
	for {
		input := &ec2.DescribeImagesInput{
			ImageIds: []string{imageID},
		}

		result, err := client.DescribeImages(ctx, input)
		if err != nil {
			var apiErr smithy.APIError
			if errors.As(err, &apiErr) {
				if apiErr.ErrorCode() == "InvalidAMIID.NotFound" {
					// Image not found, proceed
					return nil
				} else {
					// Handle other API errors
					return fmt.Errorf("API error describing image: %w", err)
				}
			} else {
				// Non-API error occurred
				return fmt.Errorf("unexpected error describing image: %w", err)
			}
		}

		if len(result.Images) == 0 {
			// Image not found, proceed
			return nil
		}

		image := result.Images[0]

		if image.State == ec2types.ImageStateDeregistered {
			// Image is deregistered, proceed
			return nil
		}

		// Wait before retrying
		time.Sleep(5 * time.Second)
	}
}
