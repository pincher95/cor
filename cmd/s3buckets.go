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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanS3Bucket struct {
	BucketName         string
	Region             string
	CreationDate       string
	IsEmpty            bool
	IncompleteUploads  int
	HasLifecyclePolicy bool
	Reason             string
}

// s3bucketsCmd represents the s3buckets command
var s3bucketsCmd = &cobra.Command{
	Use:   "s3buckets",
	Short: "List orphaned S3 buckets",
	Long: `Finds S3 buckets that are potentially orphaned based on:
- Empty buckets (zero objects)
- Buckets with incomplete multipart uploads
- Buckets without lifecycle policies (optional flag)

Orphaned S3 buckets can incur costs:
- Storage: $0.023/GB-month (Standard)
- Request costs accumulate even for empty buckets
- Incomplete multipart uploads consume storage

WARNING: Checking large buckets can be slow. This command focuses on
empty buckets and incomplete uploads for efficiency.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "check-lifecycle", Type: "bool"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{S3: s3.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeS3Buckets)
	},
}

func init() {
	s3bucketsCmd.Flags().Bool("check-lifecycle", false, "Also flag buckets without lifecycle policies")
}

func (a *AWSCommand) executeS3Buckets(ctx context.Context, flagValues *map[string]any) error {
	checkLifecycle := (*flagValues)["check-lifecycle"].(bool)

	return runOrphanPipeline(a, ctx, flagValues, OrphanPipeline[s3types.Bucket, orphanS3Bucket]{
		Headers:       []string{"Bucket Name", "Region", "Created", "Empty", "Incomplete Uploads", "Has Lifecycle", "Reason"},
		ResourceLabel: "S3 buckets",
		HideIndex:     true,
		List: func(ctx context.Context, emit func(s3types.Bucket) error) error {
			out, err := a.AWSClient.S3.ListBuckets(ctx, &s3.ListBucketsInput{})
			if err != nil {
				return err
			}
			for _, b := range out.Buckets {
				if err := emit(b); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(ctx context.Context, b s3types.Bucket) (*orphanS3Bucket, error) {
			orphan, err := a.checkS3BucketOrphan(ctx, b, checkLifecycle)
			if err != nil {
				a.Logger.LogError("Error checking S3 bucket", err, map[string]any{
					"bucket": aws.ToString(b.Name),
				}, false)
				return nil, nil
			}
			return orphan, nil
		},
		ToRow: func(r orphanS3Bucket) []any {
			yesNo := func(b bool) string {
				if b {
					return "Yes"
				}
				return "No"
			}
			return []any{
				r.BucketName,
				r.Region,
				r.CreationDate,
				yesNo(r.IsEmpty),
				r.IncompleteUploads,
				yesNo(r.HasLifecyclePolicy),
				r.Reason,
			}
		},
		Delete: func(ctx context.Context, r orphanS3Bucket) error {
			if r.IncompleteUploads > 0 {
				if err := a.abortMultipartUploads(ctx, r.BucketName, r.Region); err != nil {
					a.Logger.LogError("Failed to abort multipart uploads", err, map[string]any{"bucket": r.BucketName}, false)
					return err
				}
			}
			if !r.IsEmpty {
				a.Logger.LogInfo("Skipped non-empty S3 bucket", map[string]any{"bucket": r.BucketName})
				return nil
			}
			a.Logger.LogInfo("Deleting S3 bucket", map[string]any{"bucket": r.BucketName})
			if err := a.deleteS3Bucket(ctx, r.BucketName, r.Region); err != nil {
				a.Logger.LogError("Failed to delete S3 bucket", err, map[string]any{"bucket": r.BucketName}, false)
				return err
			}
			return nil
		},
	})
}

func (a *AWSCommand) checkS3BucketOrphan(ctx context.Context, bucket s3types.Bucket, checkLifecycle bool) (*orphanS3Bucket, error) {
	bucketName := aws.ToString(bucket.Name)

	// Get bucket location first using the default client
	locationOutput, err := a.AWSClient.S3.GetBucketLocation(ctx, &s3.GetBucketLocationInput{
		Bucket: bucket.Name,
	})
	if err != nil {
		return nil, err
	}

	region := string(locationOutput.LocationConstraint)
	if region == "" {
		region = "us-east-1" // Default for no constraint
	}

	// Create a region-specific S3 client for this bucket
	regionalConfig := &handlers.CloudConfig{
		AuthMethod: a.CloudConfig.AuthMethod,
		Profile:    a.CloudConfig.Profile,
		Region:     aws.String(region),
	}
	cfg, err := handlers.NewConfig(ctx, *regionalConfig, "UTC", true, true)
	if err != nil {
		return nil, err
	}
	regionalS3Client := s3.NewFromConfig(*cfg)

	// Check if bucket is empty (list first object only for efficiency)
	listOutput, err := regionalS3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  bucket.Name,
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return nil, err
	}

	isEmpty := listOutput.KeyCount == nil || *listOutput.KeyCount == 0

	// Check for incomplete multipart uploads
	mpuOutput, err := regionalS3Client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: bucket.Name,
	})
	if err != nil {
		return nil, err
	}

	incompleteUploads := len(mpuOutput.Uploads)

	// Check lifecycle policy if requested
	hasLifecycle := false
	if checkLifecycle {
		_, err := regionalS3Client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{
			Bucket: bucket.Name,
		})
		hasLifecycle = err == nil
	}

	// Determine if orphaned
	isOrphan := isEmpty || incompleteUploads > 0 || (checkLifecycle && !hasLifecycle && !isEmpty)

	if !isOrphan {
		return nil, nil
	}

	creationDate := "N/A"
	if bucket.CreationDate != nil {
		creationDate = bucket.CreationDate.Format("2006-01-02")
	}

	reason := ""
	if isEmpty && incompleteUploads > 0 {
		reason = fmt.Sprintf("Empty with %d incomplete uploads", incompleteUploads)
	} else if isEmpty {
		reason = "Empty bucket"
	} else if incompleteUploads > 0 {
		reason = fmt.Sprintf("%d incomplete multipart uploads", incompleteUploads)
	} else if checkLifecycle && !hasLifecycle {
		reason = "No lifecycle policy"
	}

	return &orphanS3Bucket{
		BucketName:         bucketName,
		Region:             region,
		CreationDate:       creationDate,
		IsEmpty:            isEmpty,
		IncompleteUploads:  incompleteUploads,
		HasLifecyclePolicy: hasLifecycle,
		Reason:             reason,
	}, nil
}

func (a *AWSCommand) abortMultipartUploads(ctx context.Context, bucketName string, region string) error {
	// Create a region-specific S3 client
	regionalConfig := &handlers.CloudConfig{
		AuthMethod: a.CloudConfig.AuthMethod,
		Profile:    a.CloudConfig.Profile,
		Region:     aws.String(region),
	}
	cfg, err := handlers.NewConfig(ctx, *regionalConfig, "UTC", true, true)
	if err != nil {
		return err
	}
	regionalS3Client := s3.NewFromConfig(*cfg)

	mpuOutput, err := regionalS3Client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return err
	}

	for _, upload := range mpuOutput.Uploads {
		_, err := regionalS3Client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(bucketName),
			Key:      upload.Key,
			UploadId: upload.UploadId,
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *AWSCommand) deleteS3Bucket(ctx context.Context, bucketName string, region string) error {
	// Create a region-specific S3 client
	regionalConfig := &handlers.CloudConfig{
		AuthMethod: a.CloudConfig.AuthMethod,
		Profile:    a.CloudConfig.Profile,
		Region:     aws.String(region),
	}
	cfg, err := handlers.NewConfig(ctx, *regionalConfig, "UTC", true, true)
	if err != nil {
		return err
	}
	regionalS3Client := s3.NewFromConfig(*cfg)

	_, err = regionalS3Client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucketName),
	})
	return err
}
