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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/jedib0t/go-pretty/v6/table"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/printer"
	"github.com/pincher95/cor/pkg/handlers/prompter"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
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
		prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
		output := os.Stdout
		ctx := cmd.Context()
		logger := logging.NewLogger()

		flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
		additionalFlags := []flags.Flag{
			{Name: "check-lifecycle", Type: "bool"},
		}

		flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
		if err != nil {
			logger.LogError("Error getting flags", err, nil, true)
			return err
		}

		cloudConfig := &handlers.CloudConfig{
			AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
			Profile:    aws.String((*flagValues)["profile"].(string)),
			Region:     aws.String((*flagValues)["region"].(string)),
		}
		cfg, err := handlers.NewConfig(ctx, *cloudConfig, "UTC", true, true)
		if err != nil {
			logger.LogError("Failed loading AWS client config", err, nil, true)
			return err
		}

		s3Client := s3.NewFromConfig(*cfg)

		awsClient := &handlers.AWSClientImpl{}
		awsClient.S3 = s3Client

		return runS3BucketsCmd(ctx, &prompterClient, output, awsClient, flagValues, logger, cloudConfig)
	},
}

func init() {
	s3bucketsCmd.Flags().Bool("check-lifecycle", false, "Also flag buckets without lifecycle policies")
}

func runS3BucketsCmd(ctx context.Context, prompter *prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any, logger *logging.Logger, cloudConfig *handlers.CloudConfig) error {
	command := &AWSCommand{
		AWSClient: *awsClient,
		Logger:    logger,
		Prompter:  *prompter,
		Output:    output,
	}

	return command.executeS3Buckets(ctx, flagValues, cloudConfig)
}

func (a *AWSCommand) executeS3Buckets(ctx context.Context, flagValues *map[string]any, cloudConfig *handlers.CloudConfig) error {
	rootCtx := ctx
	collectDeletes := (*flagValues)["delete"].(bool)
	checkLifecycle := (*flagValues)["check-lifecycle"].(bool)

	bucketChan := make(chan s3types.Bucket, 50)
	resultsChan := make(chan table.Row, 50)
	orphanBuckets := []orphanS3Bucket{}

	g, egCtx := errgroup.WithContext(ctx)

	// Goroutine to list all S3 buckets
	g.Go(func() error {
		defer close(bucketChan)
		output, err := a.AWSClient.S3.ListBuckets(egCtx, &s3.ListBucketsInput{})
		if err != nil {
			a.Logger.LogError("Error listing S3 buckets", err, nil, false)
			return err
		}
		for _, bucket := range output.Buckets {
			select {
			case bucketChan <- bucket:
			case <-egCtx.Done():
				return egCtx.Err()
			}
		}
		return nil
	})

	// Worker goroutines to check each bucket
	numWorkers := NumGoroutines
	for range numWorkers {
		g.Go(func() error {
			for {
				select {
				case <-egCtx.Done():
					return nil
				case bucket, ok := <-bucketChan:
					if !ok {
						return nil
					}

					orphan, err := a.checkS3BucketOrphan(egCtx, bucket, checkLifecycle, cloudConfig)
					if err != nil {
						a.Logger.LogError("Error checking S3 bucket", err, map[string]any{
							"bucket": aws.ToString(bucket.Name),
						}, false)
						continue
					}

					if orphan != nil {
						emptyStatus := "No"
						if orphan.IsEmpty {
							emptyStatus = "Yes"
						}
						lifecycleStatus := "No"
						if orphan.HasLifecyclePolicy {
							lifecycleStatus = "Yes"
						}

						select {
						case resultsChan <- table.Row{
							orphan.BucketName,
							orphan.Region,
							orphan.CreationDate,
							emptyStatus,
							orphan.IncompleteUploads,
							lifecycleStatus,
							orphan.Reason,
						}:
						case <-egCtx.Done():
							return egCtx.Err()
						}
						orphanBuckets = append(orphanBuckets, *orphan)
					}
				}
			}
		})
	}

	// Goroutine to collect results and print
	headers := []string{"Bucket Name", "Region", "Created", "Empty", "Incomplete Uploads", "Has Lifecycle", "Reason"}

	t := printer.NewStreamTable(a.Output, false, headers)
	defer t.Close()

	go func() {
		for row := range resultsChan {
			t.WriteRow(row...)
		}
	}()

	if err := g.Wait(); err != nil {
		close(resultsChan)
		return err
	}
	close(resultsChan)
	time.Sleep(100 * time.Millisecond) // Give goroutine time to finish writing

	a.Logger.LogInfo(fmt.Sprintf("Found %d orphaned S3 buckets", len(orphanBuckets)), nil)

	if collectDeletes && len(orphanBuckets) > 0 {
		a.Logger.LogInfo("WARNING: S3 bucket deletion requires buckets to be empty. Aborting multipart uploads first...", nil)

		confirm, err := confirmDelete(a.Prompter, a.Logger)
		if err != nil || !confirm {
			return err
		}

		for _, bucket := range orphanBuckets {
			// First, abort incomplete multipart uploads
			if bucket.IncompleteUploads > 0 {
				if err := a.abortMultipartUploads(rootCtx, bucket.BucketName, bucket.Region, cloudConfig); err != nil {
					a.Logger.LogError("Failed to abort multipart uploads", err, map[string]any{
						"bucket": bucket.BucketName,
					}, false)
					continue
				}
			}

			// Only delete if bucket is empty
			if bucket.IsEmpty {
				if err := a.deleteS3Bucket(rootCtx, bucket.BucketName, bucket.Region, cloudConfig); err != nil {
					a.Logger.LogError("Failed to delete S3 bucket", err, map[string]any{
						"bucket": bucket.BucketName,
					}, false)
					continue
				}
				a.Logger.LogInfo(fmt.Sprintf("Deleted S3 bucket: %s", bucket.BucketName), nil)
			} else {
				a.Logger.LogInfo(fmt.Sprintf("Skipped non-empty bucket: %s", bucket.BucketName), nil)
			}
		}
	}

	return nil
}

func (a *AWSCommand) checkS3BucketOrphan(ctx context.Context, bucket s3types.Bucket, checkLifecycle bool, cloudConfig *handlers.CloudConfig) (*orphanS3Bucket, error) {
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
		AuthMethod: cloudConfig.AuthMethod,
		Profile:    cloudConfig.Profile,
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

func (a *AWSCommand) abortMultipartUploads(ctx context.Context, bucketName string, region string, cloudConfig *handlers.CloudConfig) error {
	// Create a region-specific S3 client
	regionalConfig := &handlers.CloudConfig{
		AuthMethod: cloudConfig.AuthMethod,
		Profile:    cloudConfig.Profile,
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

func (a *AWSCommand) deleteS3Bucket(ctx context.Context, bucketName string, region string, cloudConfig *handlers.CloudConfig) error {
	// Create a region-specific S3 client
	regionalConfig := &handlers.CloudConfig{
		AuthMethod: cloudConfig.AuthMethod,
		Profile:    cloudConfig.Profile,
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
