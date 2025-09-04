package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
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
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// imagesCmd represents the images command
var imagesCmd = &cobra.Command{
	Use:   "images",
	Short: "A brief description of your command",
	Long:  ``,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Create prompter using the prompter package
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout

		// Create a new context
		ctx := cmd.Context()

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
		}

		flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
		if err != nil {
			return err
		}

		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
			Profile:    aws.String((*flagValues)["profile"].(string)),
			Region:     aws.String((*flagValues)["region"].(string)),
		}
		// Create a new AWS client
		cfg, err := handlers.NewConfig(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			return err
		}

		// Create a new EC2 client
		ec2Client := ec2.NewFromConfig(*cfg)
		// Create a new STS client
		stsClient := sts.NewFromConfig(*cfg)

		awsClient := &handlers.AWSClientImpl{
			EC2: ec2Client,
			STS: stsClient,
		}

		return runImagesCmd(ctx, prompterClient, output, awsClient, flagValues)
	},
}

func runImagesCmd(ctx context.Context, prompter prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logging.NewLogger(),
		Prompter:  prompter,
		Output:    output,
	}

	return command.executeImages(ctx, flagValues)
}

func (i *AWSCommand) executeImages(ctx context.Context, flagValues *map[string]any) error {
	// preserve the original context for post-processing
	rootCtx := ctx
	// Create a channels to process images concurrently
	imagesChan := make(chan ec2types.Image, 10)
	resultsChan := make(chan table.Row, 10)

	// Parse the creation date flags
	var beforeCreationDate, afterCreationDate *time.Time
	dateLayout := "2006-01-02"
	if (*flagValues)["creation-date-before"].(string) != "" {
		t, err := time.Parse(dateLayout, (*flagValues)["creation-date-before"].(string))
		if err != nil {
			i.Logger.LogError("Error parsing creation date", err, nil, false)
			return err
		}
		beforeCreationDate = &t
	}
	if (*flagValues)["creation-date-after"].(string) != "" {
		t, err := time.Parse(dateLayout, (*flagValues)["creation-date-after"].(string))
		if err != nil {
			i.Logger.LogError("Error parsing creation date", err, nil, false)
			return err
		}
		afterCreationDate = &t
	}

	// Create an errgroup with a derived context for cancellation
	g, egCtx := errgroup.WithContext(ctx)

	// Get the account ID
	ownerID, err := i.AWSClient.STS.GetCallerIdentity(egCtx, &sts.GetCallerIdentityInput{})
	if err != nil {
		i.Logger.LogError("Error getting caller identity", err, nil, false)
		return err
	}

	// Goroutine to describe volumes
	g.Go(func() error {
		imageFilter := []ec2types.Filter{
			{
				Name:   aws.String("owner-id"),
				Values: []string{*ownerID.Account},
			},
			{
				Name: aws.String("name"),
				Values: func() []string {
					if filterByName, ok := (*flagValues)["filter-by-name"].(string); ok {
						return []string{filterByName}
					}
					return []string{}
				}(),
			},
		}

		if err := i.describeImages(egCtx, imagesChan, &imageFilter, beforeCreationDate, afterCreationDate); err != nil {
			return err
		}
		return nil
	})

	// Start worker pool for processing images concurrently
	for range NumGoroutines {
		g.Go(func() error {
			for {
				select {
				case <-egCtx.Done():
					return egCtx.Err()
				case image, ok := <-imagesChan:
					if !ok {
						return nil
					}
					err := i.handleImage(egCtx, image, flagValues, resultsChan)
					if err != nil {
						return err
					}
				}
			}
		})
	}

	// Result collector goroutine: concurrently reads from resultsChan.
	resultCollectorDone := make(chan struct{})
	tableRows := make([]table.Row, 0)
	go func() {
		for imageRow := range resultsChan {
			tableRows = append(tableRows, imageRow)
		}

		// Print the image table
		if err := printImageTable(&tableRows, flagValues); err != nil {
			i.Logger.LogError("Error printing table", err, nil, false)
			close(resultCollectorDone)
			return
		}

		if (*flagValues)["delete"].(bool) && len(tableRows) > 0 {
			if err := i.deleteImages(rootCtx, &tableRows); err != nil {
				i.Logger.LogError("Error deleting images", err, nil, false)
				close(resultCollectorDone)
				return
			}
		}

		close(resultCollectorDone)
	}()

	// Wait for the describer and workers to finish.
	if err := g.Wait(); err != nil {
		i.Logger.LogError("Error during image processing", err, nil, false)
		return err
	}

	// All worker and describer goroutines are done; close the results channel.
	close(resultsChan)
	// Wait for the collector to finish.
	<-resultCollectorDone

	return nil
}

func init() {
	imagesCmd.PersistentFlags().String("creation-date-before", "", "The time when the image was created, in the UTC time zone (YYYY-MM-DD), for example, 2021-09-29.")

	imagesCmd.PersistentFlags().String("creation-date-after", "", "The time when the image was created, in the UTC time zone (YYYY-MM-DD), for example, 2021-09-29.")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// imagesCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
	imagesCmd.Flags().String("filter-by-name", "*", "The name of the AMI (provided during image creation) ,wildcard ( * ) can be used.")
	imagesCmd.Flags().Bool("include-used-by-instance", false, "Include images that are used by instances.")
	imagesCmd.Flags().Bool("include-used-by-launch-template", false, "Include images that are used by launch templates.")
}

func (i *AWSCommand) describeInstances(ctx context.Context, instanceImageID *string) ([]string, error) {
	useedByIstances := make([]string, 0)
	paginator := ec2.NewDescribeInstancesPaginator(i.AWSClient.EC2, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("image-id"),
				Values: []string{*instanceImageID},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"running", "pending", "stopping", "stopped"},
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

func (i *AWSCommand) describeLaunchTemplates(ctx context.Context, imageID *string) ([]string, error) {
	usedByLaunchTemplates := make([]string, 0)
	paginatorLaunchTemplate := ec2.NewDescribeLaunchTemplatesPaginator(i.AWSClient.EC2, &ec2.DescribeLaunchTemplatesInput{
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
				paginatorLaunchTemplateVersion := ec2.NewDescribeLaunchTemplateVersionsPaginator(i.AWSClient.EC2, &ec2.DescribeLaunchTemplateVersionsInput{
					Filters: []ec2types.Filter{
						{
							Name:   aws.String("image-id"),
							Values: []string{*imageID},
						},
					},
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

func (i *AWSCommand) describeImages(ctx context.Context, imagesChan chan<- ec2types.Image, filters *[]ec2types.Filter, beforeCreationDate, afterCreationDate *time.Time) error {
	defer close(imagesChan)

	paginator := ec2.NewDescribeImagesPaginator(i.AWSClient.EC2, &ec2.DescribeImagesInput{
		Filters: *filters,
		Owners:  []string{"self"},
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}

		for _, image := range page.Images {
			imageCreationDate, err := time.Parse(time.RFC3339, *image.CreationDate)
			if err != nil {
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
					if imageCreationDate.Before(*beforeCreationDate) && imageCreationDate.After(*afterCreationDate) {
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

	return nil
}

func (i *AWSCommand) waitForImageDeregistration(ctx context.Context, imageID string) error {
	for {
		input := &ec2.DescribeImagesInput{
			ImageIds: []string{imageID},
		}

		result, err := i.AWSClient.EC2.DescribeImages(ctx, input)
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

func (i *AWSCommand) deleteImages(ctx context.Context, tableRows *[]table.Row) error {
	confirm, err := i.Prompter.Confirm("Are you sure you want to proceed? (yes/no): ")
	if err != nil {
		i.Logger.LogError("Error during user prompt", err, nil, false)
		return err
	}

	if confirm == nil {
		i.Logger.LogInfo("Invalid response. Please enter 'yes' or 'no'.", nil)
	} else if *confirm {
		for _, tableRow := range *tableRows {
			_, err := i.AWSClient.EC2.DeregisterImage(ctx, &ec2.DeregisterImageInput{
				ImageId: aws.String(tableRow[1].(string)),
				// DryRun:  aws.Bool(!deleteImages),
				// DryRun: aws.Bool(true),
			})
			if err != nil {
				i.Logger.LogError("Error deregistering image", err, nil, false)
			}

			// Verify that the image has been deregistered
			err = i.waitForImageDeregistration(ctx, tableRow[1].(string))
			if err != nil {
				i.Logger.LogError("Error verifying image deregistration", err, nil, false)
			}
			i.Logger.LogInfo("Verified image has been deregistered", map[string]any{"imageID": tableRow[1].(string)})

			for _, snapshot := range strings.Split(tableRow[3].(string), "\n") {
				_, err := i.AWSClient.EC2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
					SnapshotId: aws.String(snapshot),
					// DryRun:     aws.Bool(!deleteImages),
					// DryRun: aws.Bool(true),
				})
				if err != nil {
					i.Logger.LogError("Error deleting snapshot", err, nil, false)
				}
			}
		}

	} else if !*confirm {
		i.Logger.LogInfo("Aborted.", nil)
	}
	return nil
}

func (i *AWSCommand) handleImage(ctx context.Context, image ec2types.Image, flagValues *map[string]any, resultsChan chan<- table.Row) error {
	// Check required fields for nil.
	if image.Name == nil || image.ImageId == nil || image.CreationDate == nil {
		return errors.New("missing required image fields")
	}

	snapshotIds := getSnapshotIds(image)

	var usedByInstances, usedByLaunchTemplates []string
	var err error

	// Fetch instances using the image
	usedByInstances, err = i.describeInstances(ctx, image.ImageId)
	if err != nil {
		return err
	}

	// Fetch launch templates using the image
	usedByLaunchTemplates, err = i.describeLaunchTemplates(ctx, image.ImageId)
	if err != nil {
		return err
	}

	// Apply the filtering rules based on the flags
	// Case 1: Default behavior (no flags set)
	if !(*flagValues)["include-used-by-instance"].(bool) && !(*flagValues)["include-used-by-launch-template"].(bool) {
		// Only add if the image is NOT used by instances or launch templates
		if len(usedByInstances) == 0 && len(usedByLaunchTemplates) == 0 {
			resultsChan <- table.Row{
				*image.Name, *image.ImageId, *image.CreationDate, strings.Join(snapshotIds, "\n")}
		}
	}

	// Case 2: --include-used-by-instance is true
	if (*flagValues)["include-used-by-instance"].(bool) && !(*flagValues)["include-used-by-launch-template"].(bool) {
		if len(usedByInstances) > 0 || (len(usedByInstances) == 0 && len(usedByLaunchTemplates) == 0) {
			resultsChan <- table.Row{
				*image.Name, *image.ImageId, *image.CreationDate, strings.Join(snapshotIds, "\n"), strings.Join(usedByInstances, "\n")}
		}
	}

	// Case 3: --include-used-by-launch-template is true
	if (*flagValues)["include-used-by-launch-template"].(bool) && !(*flagValues)["include-used-by-instance"].(bool) {
		if len(usedByLaunchTemplates) > 0 || (len(usedByInstances) == 0 && len(usedByLaunchTemplates) == 0) {
			resultsChan <- table.Row{
				*image.Name, *image.ImageId, *image.CreationDate, strings.Join(snapshotIds, "\n"), strings.Join(usedByLaunchTemplates, "\n")}
		}
	}

	// Case 4: Both --include-used-by-instance and --include-used-by-launch-template are true
	if (*flagValues)["include-used-by-instance"].(bool) && (*flagValues)["include-used-by-launch-template"].(bool) {
		if len(usedByInstances) > 0 || len(usedByLaunchTemplates) > 0 || (len(usedByInstances) == 0 && len(usedByLaunchTemplates) == 0) {
			resultsChan <- table.Row{
				*image.Name, *image.ImageId, *image.CreationDate, strings.Join(snapshotIds, "\n"),
				strings.Join(usedByInstances, "\n"), strings.Join(usedByLaunchTemplates, "\n")}
		}
	}

	return nil
}

func getSnapshotIds(image ec2types.Image) []string {
	snapshotIds := make([]string, 0, len(image.BlockDeviceMappings))
	for _, mapping := range image.BlockDeviceMappings {
		if mapping.Ebs != nil && mapping.Ebs.SnapshotId != nil {
			snapshotIds = append(snapshotIds, *mapping.Ebs.SnapshotId)
		}
	}
	return snapshotIds
}

// printVolumeTable prints the volume table
func printImageTable(tableRows *[]table.Row, flagValues *map[string]any) error {

	tableRowHeader := table.Row{"ami name", "ami id", "creation date", "snapshot ids", "used by Instance", "used by Launch Template"}
	tableColumnConfig := []table.ColumnConfig{
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

	if !(*flagValues)["include-used-by-instance"].(bool) && !(*flagValues)["include-used-by-launch-template"].(bool) {
		// Only add if the image is NOT used by instances or launch templates
		tableRowHeader = table.Row{"ami name", "ami id", "creation date", "snapshot ids"}
		tableColumnConfig = []table.ColumnConfig{
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
		}
	}

	if (*flagValues)["include-used-by-instance"].(bool) && !(*flagValues)["include-used-by-launch-template"].(bool) {
		// Only add if the image is used by instances and NOT launch templates
		tableRowHeader = table.Row{"ami name", "ami id", "creation date", "snapshot ids", "used by Instance"}
		tableColumnConfig = []table.ColumnConfig{
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
		}
	}

	if !(*flagValues)["include-used-by-instance"].(bool) && (*flagValues)["include-used-by-launch-template"].(bool) {
		// Only add if the image is used by launch templates and NOT used by instances
		tableRowHeader = table.Row{"ami name", "ami id", "creation date", "snapshot ids", "used by Launch Template"}
		tableColumnConfig = []table.ColumnConfig{
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
				Name:        "used by Launch Template",
				AlignHeader: text.AlignCenter,
			},
		}
	}

	return printTable(&tableColumnConfig, &tableRowHeader, tableRows, &[]table.SortBy{{Name: "creation date", Mode: table.Asc}})
}
