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
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

type imageUsage struct {
	count    int
	examples []string
}

type orphanImage struct {
	name             string
	id               string
	creationDate     string
	snapshotIDs      []string
	usedByInstance   string
	usedByLaunchTmpl string
}

var imagesCmd = &cobra.Command{
	Use:   "images",
	Short: "List and optionally delete orphan AMIs (and their snapshots)",
	Long:  `List Amazon Machine Images (AMIs) owned by this account and identify those not used by instances or launch templates.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "creation-date-before", Type: "string"},
				{Name: "creation-date-after", Type: "string"},
				{Name: "include-used-by-instance", Type: "bool"},
				{Name: "include-used-by-launch-template", Type: "bool"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeImages)
	},
}

func init() {
	imagesCmd.PersistentFlags().String("creation-date-before", "", "Only include AMIs created before this UTC date (YYYY-MM-DD).")
	imagesCmd.PersistentFlags().String("creation-date-after", "", "Only include AMIs created after this UTC date (YYYY-MM-DD).")
	imagesCmd.Flags().String("filter-by-name", "", "Filter AMIs by name (empty = no filter).")
	imagesCmd.Flags().Bool("include-used-by-instance", false, "Include images that are used by instances.")
	imagesCmd.Flags().Bool("include-used-by-launch-template", false, "Include images that are used by launch templates.")
}

func (a *AWSCommand) executeImages(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	includeUsedByInstance := (*extras)["include-used-by-instance"].(bool)
	includeUsedByLaunchTemplate := (*extras)["include-used-by-launch-template"].(bool)
	filterByName := normalizeFilterValue((*extras)["filter-by-name"].(string))

	var beforeCreationDate, afterCreationDate *time.Time
	dateLayout := "2006-01-02"
	if s := (*extras)["creation-date-before"].(string); s != "" {
		t, err := time.Parse(dateLayout, s)
		if err != nil {
			return fmt.Errorf("invalid creation-date-before: %w", err)
		}
		beforeCreationDate = &t
	}
	if s := (*extras)["creation-date-after"].(string); s != "" {
		t, err := time.Parse(dateLayout, s)
		if err != nil {
			return fmt.Errorf("invalid creation-date-after: %w", err)
		}
		afterCreationDate = &t
	}

	usedByInstances := make(map[string]*imageUsage, 1024)
	usedByLaunchTemplates := make(map[string]*imageUsage, 1024)
	var instMu, ltMu sync.Mutex

	headers := []string{"ami name", "ami id", "creation date", "snapshot ids"}
	if includeUsedByInstance && !includeUsedByLaunchTemplate {
		headers = append(headers, "used by Instance")
	}
	if !includeUsedByInstance && includeUsedByLaunchTemplate {
		headers = append(headers, "used by Launch Template")
	}
	if includeUsedByInstance && includeUsedByLaunchTemplate {
		headers = append(headers, "used by Instance", "used by Launch Template")
	}

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[ec2types.Image, orphanImage]{
		Headers:       headers,
		ResourceLabel: "AMIs",
		PreScan: func(ctx context.Context) error {
			return a.scanImageUsage(ctx, scanImageUsageOpts{
				includeUsedByInstance:       includeUsedByInstance,
				includeUsedByLaunchTemplate: includeUsedByLaunchTemplate,
				instances:                   usedByInstances,
				launchTemplates:             usedByLaunchTemplates,
				instMu:                      &instMu,
				ltMu:                        &ltMu,
			})
		},
		List: func(ctx context.Context, emit func(ec2types.Image) error) error {
			imageFilters := []ec2types.Filter{}
			if filterByName != "" {
				imageFilters = append(imageFilters, ec2types.Filter{
					Name:   aws.String("name"),
					Values: []string{filterByName},
				})
			}
			p := ec2.NewDescribeImagesPaginator(a.AWSClient.EC2, &ec2.DescribeImagesInput{
				Owners:  []string{"self"},
				Filters: imageFilters,
			})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, img := range page.Images {
					if err := emit(img); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, img ec2types.Image) (*orphanImage, error) {
			if img.ImageId == nil || img.Name == nil || img.CreationDate == nil {
				return nil, nil
			}
			imageID := *img.ImageId
			usedInst := usedByInstances[imageID] != nil && usedByInstances[imageID].count > 0
			usedLT := usedByLaunchTemplates[imageID] != nil && usedByLaunchTemplates[imageID].count > 0

			// Show an image when the user-allowed columns cover its usage:
			// orphans (neither flag) are always allowed; instance-used rows
			// require --include-used-by-instance; LT-used rows require
			// --include-used-by-launch-template.
			if (usedInst && !includeUsedByInstance) || (usedLT && !includeUsedByLaunchTemplate) {
				return nil, nil
			}

			createdAt, err := time.Parse(time.RFC3339, *img.CreationDate)
			if err != nil {
				return nil, nil
			}
			if beforeCreationDate != nil && createdAt.After(*beforeCreationDate) {
				return nil, nil
			}
			if afterCreationDate != nil && createdAt.Before(*afterCreationDate) {
				return nil, nil
			}

			return &orphanImage{
				name:             *img.Name,
				id:               imageID,
				creationDate:     *img.CreationDate,
				snapshotIDs:      getSnapshotIds(img),
				usedByInstance:   formatUsage(usedByInstances[imageID]),
				usedByLaunchTmpl: formatUsage(usedByLaunchTemplates[imageID]),
			}, nil
		},
		ToRow: func(r orphanImage) []any {
			base := []any{r.name, r.id, r.creationDate, strings.Join(r.snapshotIDs, "\n")}
			switch {
			case includeUsedByInstance && includeUsedByLaunchTemplate:
				base = append(base, r.usedByInstance, r.usedByLaunchTmpl)
			case includeUsedByInstance:
				base = append(base, r.usedByInstance)
			case includeUsedByLaunchTemplate:
				base = append(base, r.usedByLaunchTmpl)
			}
			return base
		},
		Delete: func(ctx context.Context, r orphanImage) error {
			a.Logger.LogInfo("Deleting AMI", map[string]any{"amiID": r.id})
			if _, err := a.AWSClient.EC2.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: aws.String(r.id)}); err != nil {
				return err
			}
			if err := a.waitForImageDeregistration(ctx, r.id); err != nil {
				return err
			}
			for _, snapshot := range r.snapshotIDs {
				if snapshot == "" {
					continue
				}
				if _, err := a.AWSClient.EC2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapshot)}); err != nil {
					return err
				}
			}
			return nil
		},
		DeleteConcurrency: 5,
		DedupKey:          func(r orphanImage) string { return r.id },
	})
}

type scanImageUsageOpts struct {
	includeUsedByInstance       bool
	includeUsedByLaunchTemplate bool
	instances                   map[string]*imageUsage
	launchTemplates             map[string]*imageUsage
	instMu                      *sync.Mutex
	ltMu                        *sync.Mutex
}

func (a *AWSCommand) scanImageUsage(ctx context.Context, opts scanImageUsageOpts) error {
	g, gctx := errgroup.WithContext(ctx)

	addUsage := func(m map[string]*imageUsage, mu *sync.Mutex, imageID, example string) {
		if imageID == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		u := m[imageID]
		if u == nil {
			u = &imageUsage{examples: make([]string, 0, 10)}
			m[imageID] = u
		}
		u.count++
		if example != "" && len(u.examples) < 10 {
			u.examples = append(u.examples, example)
		}
	}

	if !opts.includeUsedByLaunchTemplate || opts.includeUsedByInstance {
		g.Go(func() error {
			p := ec2.NewDescribeInstancesPaginator(a.AWSClient.EC2, &ec2.DescribeInstancesInput{
				Filters: []ec2types.Filter{
					{
						Name:   aws.String("instance-state-name"),
						Values: []string{"running", "pending", "stopping", "stopped"},
					},
				},
			})
			pages := 0
			for p.HasMorePages() {
				page, err := p.NextPage(gctx)
				if err != nil {
					return err
				}
				pages++
				if pages%25 == 0 {
					a.Logger.LogInfo("Scanning instances for AMI usage", map[string]any{"pages": pages})
				}
				for _, res := range page.Reservations {
					for _, inst := range res.Instances {
						imageID := aws.ToString(inst.ImageId)
						ex := ""
						if opts.includeUsedByInstance {
							ex = aws.ToString(inst.InstanceId)
						}
						addUsage(opts.instances, opts.instMu, imageID, ex)
					}
				}
			}
			return nil
		})
	}

	if !opts.includeUsedByInstance || opts.includeUsedByLaunchTemplate {
		g.Go(func() error {
			ltChan := make(chan ec2types.LaunchTemplate, 50)
			inner, ictx := errgroup.WithContext(gctx)

			inner.Go(func() error {
				defer close(ltChan)
				p := ec2.NewDescribeLaunchTemplatesPaginator(a.AWSClient.EC2, &ec2.DescribeLaunchTemplatesInput{})
				for p.HasMorePages() {
					page, err := p.NextPage(ictx)
					if err != nil {
						return err
					}
					for _, lt := range page.LaunchTemplates {
						select {
						case <-ictx.Done():
							return ictx.Err()
						case ltChan <- lt:
						}
					}
				}
				return nil
			})

			for range NumGoroutines {
				inner.Go(func() error {
					for {
						select {
						case <-ictx.Done():
							return ictx.Err()
						case lt, ok := <-ltChan:
							if !ok {
								return nil
							}
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
							pv := ec2.NewDescribeLaunchTemplateVersionsPaginator(a.AWSClient.EC2, &ec2.DescribeLaunchTemplateVersionsInput{
								LaunchTemplateId: aws.String(ltID),
								Versions:         []string{"$Default"},
							})
							for pv.HasMorePages() {
								vp, err := pv.NextPage(ictx)
								if err != nil {
									return err
								}
								for _, ver := range vp.LaunchTemplateVersions {
									if ver.LaunchTemplateData == nil || ver.LaunchTemplateData.ImageId == nil {
										continue
									}
									imageID := aws.ToString(ver.LaunchTemplateData.ImageId)
									ex := ""
									if opts.includeUsedByLaunchTemplate {
										ex = ltID
									}
									addUsage(opts.launchTemplates, opts.ltMu, imageID, ex)
								}
							}
						}
					}
				})
			}

			return inner.Wait()
		})
	}

	return g.Wait()
}

func formatUsage(u *imageUsage) string {
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

func (a *AWSCommand) waitForImageDeregistration(ctx context.Context, imageID string) error {
	for {
		result, err := a.AWSClient.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{
			ImageIds: []string{imageID},
		})
		if err != nil {
			var apiErr smithy.APIError
			if errors.As(err, &apiErr) {
				if apiErr.ErrorCode() == "InvalidAMIID.NotFound" {
					return nil
				}
				return fmt.Errorf("API error describing image: %w", err)
			}
			return fmt.Errorf("unexpected error describing image: %w", err)
		}
		if len(result.Images) == 0 {
			return nil
		}
		if result.Images[0].State == ec2types.ImageStateDeregistered {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
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
