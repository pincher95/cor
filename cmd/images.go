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
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
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

		awsClient := &handlers.AWSClientImpl{
			EC2: ec2Client,
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
	// preserve the original context for delete operations (avoid errgroup ctx cancellation)
	rootCtx := ctx

	// If deleting, confirm up-front so we can stream without buffering IDs.
	doDelete := false
	if (*flagValues)["delete"].(bool) {
		confirm, err := i.Prompter.Confirm("Are you sure you want to proceed? (yes/no): ")
		if err != nil {
			i.Logger.LogError("Error during user prompt", err, nil, false)
			return err
		}
		if confirm == nil || !*confirm {
			i.Logger.LogInfo("Aborted.", nil)
			return nil
		}
		doDelete = true
	}

	includeUsedByInstance := (*flagValues)["include-used-by-instance"].(bool)
	includeUsedByLaunchTemplate := (*flagValues)["include-used-by-launch-template"].(bool)

	// Streaming output header
	header := []string{"ami name", "ami id", "creation date", "snapshot ids"}
	if includeUsedByInstance && !includeUsedByLaunchTemplate {
		header = []string{"ami name", "ami id", "creation date", "snapshot ids", "used by Instance"}
	}
	if !includeUsedByInstance && includeUsedByLaunchTemplate {
		header = []string{"ami name", "ami id", "creation date", "snapshot ids", "used by Launch Template"}
	}
	if includeUsedByInstance && includeUsedByLaunchTemplate {
		header = []string{"ami name", "ami id", "creation date", "snapshot ids", "used by Instance", "used by Launch Template"}
	}
	stream := printer.NewStreamTable(i.Output, true, header)
	stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
	defer stream.Close()

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

	// -------------------------------------------------------------------------
	// Fast path: compute usage sets once (instances + launch templates),
	// instead of per-image DescribeInstances / DescribeLaunchTemplateVersions.
	// This reduces API calls from O(#images) to O(#instances + #launch-templates).
	// -------------------------------------------------------------------------
	type usage struct {
		count    int
		examples []string // bounded
	}
	const maxExamples = 10

	usedByInstances := make(map[string]*usage, 1024)       // imageID -> usage
	usedByLaunchTemplates := make(map[string]*usage, 1024) // imageID -> usage
	var instMu, ltMu sync.Mutex

	addUsage := func(m map[string]*usage, mu *sync.Mutex, imageID string, example string) {
		if imageID == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		u := m[imageID]
		if u == nil {
			u = &usage{examples: make([]string, 0, maxExamples)}
			m[imageID] = u
		}
		u.count++
		if example != "" && len(u.examples) < maxExamples {
			u.examples = append(u.examples, example)
		}
	}

	formatUsage := func(u *usage) string {
		if u == nil || u.count == 0 {
			return ""
		}
		if len(u.examples) == 0 {
			return fmt.Sprintf("%d", u.count)
		}
		if u.count > len(u.examples) {
			return strings.Join(u.examples, "\n") + fmt.Sprintf("\n...(+%d more)", u.count-len(u.examples))
		}
		return strings.Join(u.examples, "\n")
	}

	usageGroup, usageCtx := errgroup.WithContext(ctx)

	// Scan instances once (only if needed for filtering/output)
	if !includeUsedByLaunchTemplate || includeUsedByInstance {
		usageGroup.Go(func() error {
			p := ec2.NewDescribeInstancesPaginator(i.AWSClient.EC2, &ec2.DescribeInstancesInput{
				Filters: []ec2types.Filter{
					{
						Name:   aws.String("instance-state-name"),
						Values: []string{"running", "pending", "stopping", "stopped"},
					},
				},
			})
			pages := 0
			for p.HasMorePages() {
				page, err := p.NextPage(usageCtx)
				if err != nil {
					return err
				}
				pages++
				if pages%25 == 0 {
					i.Logger.LogInfo("Scanning instances for AMI usage…", map[string]any{"pages": pages})
				}
				for _, res := range page.Reservations {
					for _, inst := range res.Instances {
						imageID := aws.ToString(inst.ImageId)
						instID := aws.ToString(inst.InstanceId)
						// Only store examples when user asked for usage columns.
						ex := ""
						if includeUsedByInstance {
							ex = instID
						}
						addUsage(usedByInstances, &instMu, imageID, ex)
					}
				}
			}
			return nil
		})
	}

	// Scan launch template default versions once (only if needed for filtering/output)
	if !includeUsedByInstance || includeUsedByLaunchTemplate {
		usageGroup.Go(func() error {
			ltChan := make(chan ec2types.LaunchTemplate, 50)
			g, ltCtx := errgroup.WithContext(usageCtx)

			// Producer: list launch templates
			g.Go(func() error {
				defer close(ltChan)
				p := ec2.NewDescribeLaunchTemplatesPaginator(i.AWSClient.EC2, &ec2.DescribeLaunchTemplatesInput{})
				for p.HasMorePages() {
					page, err := p.NextPage(ltCtx)
					if err != nil {
						return err
					}
					for _, lt := range page.LaunchTemplates {
						select {
						case <-ltCtx.Done():
							return ltCtx.Err()
						case ltChan <- lt:
						}
					}
				}
				return nil
			})

			// Workers: fetch $Default versions
			for range NumGoroutines {
				g.Go(func() error {
					for {
						select {
						case <-ltCtx.Done():
							return ltCtx.Err()
						case lt, ok := <-ltChan:
							if !ok {
								return nil
							}

							// Preserve legacy exclusion: skip launch templates with any tag key containing "karpenter".
							shouldProcess := true
							for _, tag := range lt.Tags {
								if strings.Contains(aws.ToString(tag.Key), "karpenter") {
									shouldProcess = false
									break
								}
							}
							if !shouldProcess {
								continue
							}

							ltID := aws.ToString(lt.LaunchTemplateId)
							if ltID == "" {
								continue
							}

							pv := ec2.NewDescribeLaunchTemplateVersionsPaginator(i.AWSClient.EC2, &ec2.DescribeLaunchTemplateVersionsInput{
								LaunchTemplateId: aws.String(ltID),
								Versions:         []string{"$Default"},
							})
							for pv.HasMorePages() {
								vp, err := pv.NextPage(ltCtx)
								if err != nil {
									return err
								}
								for _, ver := range vp.LaunchTemplateVersions {
									if ver.LaunchTemplateData == nil || ver.LaunchTemplateData.ImageId == nil {
										continue
									}
									imageID := aws.ToString(ver.LaunchTemplateData.ImageId)
									ex := ""
									if includeUsedByLaunchTemplate {
										ex = ltID
									}
									addUsage(usedByLaunchTemplates, &ltMu, imageID, ex)
								}
							}
						}
					}
				})
			}

			return g.Wait()
		})
	}

	if err := usageGroup.Wait(); err != nil {
		i.Logger.LogError("Error collecting AMI usage", err, nil, false)
		return err
	}

	// -------------------------------------------------------------------------
	// Describe images and stream results
	// -------------------------------------------------------------------------
	filterByName := (*flagValues)["filter-by-name"].(string)
	imageFilters := []ec2types.Filter{
		{
			Name: aws.String("name"),
			Values: func() []string {
				if filterByName != "" {
					return []string{filterByName}
				}
				return []string{}
			}(),
		},
	}

	imgPaginator := ec2.NewDescribeImagesPaginator(i.AWSClient.EC2, &ec2.DescribeImagesInput{
		Owners:  []string{"self"},
		Filters: imageFilters,
	})

	for imgPaginator.HasMorePages() {
		page, err := imgPaginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, img := range page.Images {
			if img.ImageId == nil || img.Name == nil || img.CreationDate == nil {
				continue
			}

			imageID := *img.ImageId
			usedInst := usedByInstances[imageID] != nil && usedByInstances[imageID].count > 0
			usedLT := usedByLaunchTemplates[imageID] != nil && usedByLaunchTemplates[imageID].count > 0

			// Apply legacy include rules:
			// - default: show only orphans
			// - include-used-by-instance: show instance-used + orphans (exclude LT-only)
			// - include-used-by-launch-template: show LT-used + orphans (exclude instance-only)
			// - both: show all
			show := false
			switch {
			case includeUsedByInstance && includeUsedByLaunchTemplate:
				show = true
			case includeUsedByInstance && !includeUsedByLaunchTemplate:
				show = usedInst || (!usedInst && !usedLT)
			case !includeUsedByInstance && includeUsedByLaunchTemplate:
				show = usedLT || (!usedInst && !usedLT)
			default:
				show = !usedInst && !usedLT
			}
			if !show {
				continue
			}

			// Parse creation date for before/after filters
			createdAt, err := time.Parse(time.RFC3339, *img.CreationDate)
			if err != nil {
				continue
			}
			if beforeCreationDate != nil && createdAt.After(*beforeCreationDate) {
				continue
			}
			if afterCreationDate != nil && createdAt.Before(*afterCreationDate) {
				continue
			}

			snapshotIds := getSnapshotIds(img)
			base := []any{*img.Name, imageID, *img.CreationDate, strings.Join(snapshotIds, "\n")}

			if includeUsedByInstance && !includeUsedByLaunchTemplate {
				base = append(base, formatUsage(usedByInstances[imageID]))
			} else if !includeUsedByInstance && includeUsedByLaunchTemplate {
				base = append(base, formatUsage(usedByLaunchTemplates[imageID]))
			} else if includeUsedByInstance && includeUsedByLaunchTemplate {
				base = append(base, formatUsage(usedByInstances[imageID]), formatUsage(usedByLaunchTemplates[imageID]))
			}

			stream.WriteRow(base...)

			if doDelete {
				i.Logger.LogInfo("Deleting AMI", map[string]any{"amiID": imageID})
				if _, err := i.AWSClient.EC2.DeregisterImage(rootCtx, &ec2.DeregisterImageInput{ImageId: aws.String(imageID)}); err != nil {
					return err
				}

				if err := i.waitForImageDeregistration(rootCtx, imageID); err != nil {
					return err
				}

				for _, snapshot := range snapshotIds {
					if snapshot == "" {
						continue
					}
					if _, err := i.AWSClient.EC2.DeleteSnapshot(rootCtx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapshot)}); err != nil {
						return err
					}
				}
			}
		}
	}

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

// deleteImages removed: deletion is now handled in streaming mode to avoid buffering.

func getSnapshotIds(image ec2types.Image) []string {
	snapshotIds := make([]string, 0, len(image.BlockDeviceMappings))
	for _, mapping := range image.BlockDeviceMappings {
		if mapping.Ebs != nil && mapping.Ebs.SnapshotId != nil {
			snapshotIds = append(snapshotIds, *mapping.Ebs.SnapshotId)
		}
	}
	return snapshotIds
}
