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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/pincher95/cor/pkg/cost"
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
	// SizeByClass holds bytes-per-storage-class as reported by CloudWatch.
	// Cost is computed per class because IT / IA / Glacier rates differ
	// from Standard by up to 23×.
	SizeByClass map[string]int64
	Reason      string
}

// totalBytes returns the sum of bytes across all storage classes.
func (o *orphanS3Bucket) totalBytes() int64 {
	var t int64
	for _, b := range o.SizeByClass {
		t += b
	}
	return t
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
				{Name: "include-zero-cost", Type: "bool"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{S3: s3.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeS3Buckets)
	},
}

func init() {
	s3bucketsCmd.Flags().Bool("check-lifecycle", false, "Also flag buckets without lifecycle policies")
	s3bucketsCmd.Flags().Bool("include-zero-cost", false, "Include buckets with $0 estimated monthly cost (empty, no size). Hidden by default to reduce noise.")
}

func (a *AWSCommand) executeS3Buckets(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	checkLifecycle := (*extras)["check-lifecycle"].(bool)
	includeZeroCost := (*extras)["include-zero-cost"].(bool)

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[s3types.Bucket, orphanS3Bucket]{
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
				})
				return nil, nil
			}
			if orphan != nil && !includeZeroCost && orphan.totalBytes() <= 0 {
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
					a.Logger.LogError("Failed to abort multipart uploads", err, map[string]any{"bucket": r.BucketName})
					return err
				}
			}
			if !r.IsEmpty {
				a.Logger.LogInfo("Skipped non-empty S3 bucket", map[string]any{"bucket": r.BucketName})
				return nil
			}
			a.Logger.LogInfo("Deleting S3 bucket", map[string]any{"bucket": r.BucketName})
			if err := a.deleteS3Bucket(ctx, r.BucketName, r.Region); err != nil {
				a.Logger.LogError("Failed to delete S3 bucket", err, map[string]any{"bucket": r.BucketName})
				return err
			}
			return nil
		},
		MonthlyCost: func(r orphanS3Bucket) cost.USD {
			var total cost.USD
			for class, bytes := range r.SizeByClass {
				if bytes <= 0 {
					continue
				}
				gb := float64(bytes) / (1024 * 1024 * 1024)
				total += cost.USD(gb) * a.Pricing.S3StorageGB(class)
			}
			return total
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

	// Count incomplete multipart uploads. Paginate — a single ListMultipart-
	// Uploads call caps at 1000 and buckets like archivers can have far more.
	incompleteUploads := 0
	mpuPager := s3.NewListMultipartUploadsPaginator(regionalS3Client, &s3.ListMultipartUploadsInput{
		Bucket: bucket.Name,
	})
	for mpuPager.HasMorePages() {
		page, err := mpuPager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		incompleteUploads += len(page.Uploads)
	}

	// Lifecycle is always queried so the "Has Lifecycle" column is honest.
	// --check-lifecycle only controls whether absence of a policy promotes
	// the bucket to an orphan; the column itself is informational.
	lcOut, lcErr := regionalS3Client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{
		Bucket: bucket.Name,
	})
	hasLifecycle := lcErr == nil
	// abortsMPU is true when any Enabled rule with an empty-prefix filter
	// has AbortIncompleteMultipartUpload set — meaning S3 will sweep
	// dangling uploads automatically, so there is nothing for cor to flag.
	abortsMPU := false
	if hasLifecycle {
		for _, rule := range lcOut.Rules {
			if rule.Status != s3types.ExpirationStatusEnabled || rule.AbortIncompleteMultipartUpload == nil {
				continue
			}
			if lifecycleRuleCoversAllKeys(rule) {
				abortsMPU = true
				break
			}
		}
	}

	// Determine if orphaned. Buckets whose multipart uploads are already
	// being aborted by lifecycle don't need cor's attention — the cost is
	// being managed by AWS on a schedule.
	mpuActionable := incompleteUploads > 0 && !abortsMPU
	isOrphan := isEmpty || mpuActionable || (checkLifecycle && !hasLifecycle && !isEmpty)

	if !isOrphan {
		return nil, nil
	}

	creationDate := "N/A"
	if bucket.CreationDate != nil {
		creationDate = bucket.CreationDate.Format("2006-01-02")
	}

	reason := ""
	switch {
	case isEmpty && incompleteUploads > 0:
		reason = fmt.Sprintf("Empty with %d incomplete uploads", incompleteUploads)
	case isEmpty:
		reason = "Empty bucket"
	case mpuActionable:
		reason = fmt.Sprintf("%d incomplete multipart uploads (no abort lifecycle)", incompleteUploads)
	case checkLifecycle && !hasLifecycle:
		reason = "No lifecycle policy"
	}

	// Best-effort size lookup from CloudWatch BucketSizeBytes across every
	// storage class. Free of charge, daily granularity. Errors are non-
	// fatal — missing data renders as "—" in the cost column.
	sizeByClass, _ := a.s3SizeByClass(ctx, bucketName, region)

	return &orphanS3Bucket{
		BucketName:         bucketName,
		Region:             region,
		CreationDate:       creationDate,
		IsEmpty:            isEmpty,
		IncompleteUploads:  incompleteUploads,
		HasLifecyclePolicy: hasLifecycle,
		SizeByClass:        sizeByClass,
		Reason:             reason,
	}, nil
}

// lifecycleRuleCoversAllKeys returns true when the rule's filter selects
// every object in the bucket — either no filter at all or an empty prefix.
// Rules with a non-empty prefix or tag filter only catch a subset of keys
// and are not enough to guarantee bucket-wide MPU cleanup.
func lifecycleRuleCoversAllKeys(rule s3types.LifecycleRule) bool {
	if rule.Filter == nil {
		return true
	}
	f := rule.Filter
	if f.Prefix != nil && aws.ToString(f.Prefix) != "" {
		return false
	}
	if f.Tag != nil || f.ObjectSizeGreaterThan != nil || f.ObjectSizeLessThan != nil {
		return false
	}
	if f.And != nil {
		if aws.ToString(f.And.Prefix) != "" || len(f.And.Tags) > 0 ||
			f.And.ObjectSizeGreaterThan != nil || f.And.ObjectSizeLessThan != nil {
			return false
		}
	}
	return true
}

// s3StorageClasses lists every CloudWatch StorageType dimension value that
// BucketSizeBytes emits. Kept in sync with pkg/cost rate keys.
var s3StorageClasses = []string{
	"StandardStorage",
	"StandardIAStorage",
	"OneZoneIAStorage",
	"ReducedRedundancyStorage",
	"IntelligentTieringFAStorage",
	"IntelligentTieringIAStorage",
	"IntelligentTieringAAStorage",
	"IntelligentTieringAIAStorage",
	"IntelligentTieringDAAStorage",
	"GlacierInstantRetrievalStorage",
	"GlacierStorage",
	"DeepArchiveStorage",
}

// s3SizeByClass returns bytes-per-storage-class for the bucket using one
// GetMetricData call covering every CloudWatch StorageType dimension cor
// knows about (3-day lookback, daily granularity). Free, no S3 LIST.
// Classes with no datapoints are omitted from the result.
func (a *AWSCommand) s3SizeByClass(ctx context.Context, bucketName, region string) (map[string]int64, error) {
	regionalConfig := &handlers.CloudConfig{
		AuthMethod: a.CloudConfig.AuthMethod,
		Profile:    a.CloudConfig.Profile,
		Region:     aws.String(region),
	}
	cfg, err := handlers.NewConfig(ctx, *regionalConfig, "UTC", true, true)
	if err != nil {
		return nil, err
	}
	cw := cloudwatch.NewFromConfig(*cfg)

	end := time.Now()
	start := end.Add(-3 * 24 * time.Hour)
	queries := make([]cwtypes.MetricDataQuery, 0, len(s3StorageClasses))
	for i, class := range s3StorageClasses {
		queries = append(queries, cwtypes.MetricDataQuery{
			Id: aws.String(fmt.Sprintf("c%d", i)),
			MetricStat: &cwtypes.MetricStat{
				Metric: &cwtypes.Metric{
					Namespace:  aws.String("AWS/S3"),
					MetricName: aws.String("BucketSizeBytes"),
					Dimensions: []cwtypes.Dimension{
						{Name: aws.String("BucketName"), Value: aws.String(bucketName)},
						{Name: aws.String("StorageType"), Value: aws.String(class)},
					},
				},
				Period: aws.Int32(86400),
				Stat:   aws.String("Average"),
			},
			ReturnData: aws.Bool(true),
		})
	}

	out, err := cw.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		StartTime:         &start,
		EndTime:           &end,
		MetricDataQueries: queries,
	})
	if err != nil {
		return nil, err
	}

	sizes := make(map[string]int64, len(out.MetricDataResults))
	for _, r := range out.MetricDataResults {
		if r.Id == nil || len(r.Values) == 0 {
			continue
		}
		var i int
		if _, err := fmt.Sscanf(*r.Id, "c%d", &i); err != nil || i < 0 || i >= len(s3StorageClasses) {
			continue
		}
		// MetricDataResult.Values is newest-first by default — take the latest.
		latest := int64(r.Values[0])
		if latest > 0 {
			sizes[s3StorageClasses[i]] = latest
		}
	}
	return sizes, nil
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
