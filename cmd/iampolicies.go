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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanIAMPolicy struct {
	name, arn, path string
	ageDays         int
	attachmentCount int32
}

var iamPoliciesCmd = &cobra.Command{
	Use:   "iampolicies",
	Short: "List and optionally delete unattached customer-managed IAM policies",
	Long: `List customer-managed IAM policies attached to no principals and used as
no permissions boundary, and optionally delete them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "path-prefix", Type: "string"},
				{Name: "min-age-days", Type: "int"},
				{Name: "include-attached", Type: "bool"},
				{Name: "require-tag", Type: "string"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{IAM: iam.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeIAMPolicies)
	},
}

func (a *AWSCommand) executeIAMPolicies(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByName := normalizeFilterValue(getFlagString(extras, "filter-by-name"))
	pathPrefix := getFlagString(extras, "path-prefix")
	minAgeDays, _ := (*extras)["min-age-days"].(int)
	includeAttached, _ := (*extras)["include-attached"].(bool)
	requireTag := parseTagFilters(getFlagString(extras, "require-tag"))

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[iamtypes.Policy, orphanIAMPolicy]{
		Headers:       []string{"Name", "Path", "Attachments", "Age (days)"},
		ResourceLabel: "IAM customer-managed policies",
		HideIndex:     true,
		List: func(ctx context.Context, emit func(iamtypes.Policy) error) error {
			in := &iam.ListPoliciesInput{
				Scope:        iamtypes.PolicyScopeTypeLocal,
				OnlyAttached: false,
			}
			if pathPrefix != "" {
				in.PathPrefix = aws.String(pathPrefix)
			}
			p := iam.NewListPoliciesPaginator(a.AWSClient.IAM, in)
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, pol := range page.Policies {
					if err := emit(pol); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, pol iamtypes.Policy) (*orphanIAMPolicy, error) {
			name := aws.ToString(pol.PolicyName)
			if filterByName != "" && !matchesFilterValue(name, filterByName) {
				return nil, nil
			}
			attachments := aws.ToInt32(pol.AttachmentCount)
			boundaryUse := aws.ToInt32(pol.PermissionsBoundaryUsageCount)
			if !includeAttached && (attachments > 0 || boundaryUse > 0) {
				return nil, nil
			}
			ageDays := int(time.Since(aws.ToTime(pol.CreateDate)).Hours() / 24)
			if minAgeDays > 0 && ageDays < minAgeDays {
				return nil, nil
			}
			if len(requireTag) > 0 {
				tags, err := a.AWSClient.IAM.ListPolicyTags(ctx, &iam.ListPolicyTagsInput{PolicyArn: pol.Arn})
				if err != nil {
					a.Logger.LogError("ListPolicyTags failed; skipping policy", err, map[string]any{"arn": aws.ToString(pol.Arn)})
					return nil, nil
				}
				if !tagsMatchFilters(iamTagsToMap(tags.Tags), requireTag) {
					return nil, nil
				}
			}
			return &orphanIAMPolicy{
				name:            name,
				arn:             aws.ToString(pol.Arn),
				path:            aws.ToString(pol.Path),
				ageDays:         ageDays,
				attachmentCount: attachments,
			}, nil
		},
		ToRow: func(r orphanIAMPolicy) []any {
			return []any{r.name, r.path, r.attachmentCount, r.ageDays}
		},
		Delete:            a.deleteIAMPolicy,
		DeleteConcurrency: 3,
		DedupKey:          func(r orphanIAMPolicy) string { return r.arn },
	})
}

// deleteIAMPolicy deletes every non-default version before the policy itself;
// DeletePolicy fails while versions or attachments remain.
func (a *AWSCommand) deleteIAMPolicy(ctx context.Context, r orphanIAMPolicy) error {
	if r.attachmentCount > 0 {
		a.Logger.LogInfo("skipping attached IAM policy", map[string]any{"arn": r.arn, "attachments": r.attachmentCount})
		return nil
	}
	versions, err := a.AWSClient.IAM.ListPolicyVersions(ctx, &iam.ListPolicyVersionsInput{PolicyArn: aws.String(r.arn)})
	if err != nil {
		return err
	}
	for _, v := range versions.Versions {
		if v.IsDefaultVersion {
			continue
		}
		if _, err := a.AWSClient.IAM.DeletePolicyVersion(ctx, &iam.DeletePolicyVersionInput{
			PolicyArn: aws.String(r.arn),
			VersionId: v.VersionId,
		}); err != nil {
			return err
		}
	}
	a.Logger.LogInfo("Deleting IAM policy", map[string]any{"arn": r.arn})
	_, err = a.AWSClient.IAM.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: aws.String(r.arn)})
	return err
}

func iamTagsToMap(tags []iamtypes.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

func init() {
	iamPoliciesCmd.Flags().String("filter-by-name", "", "Filter policies by name substring/glob (empty = no filter).")
	iamPoliciesCmd.Flags().String("path-prefix", "", "Only list policies under this IAM path prefix (e.g. /service-role/).")
	iamPoliciesCmd.Flags().Int("min-age-days", 0, "Only report policies created at least this many days ago (0 = no age gate).")
	iamPoliciesCmd.Flags().Bool("include-attached", false, "Also show policies still attached to principals (never deleted).")
	iamPoliciesCmd.Flags().String("require-tag", "", "Safety gate: only consider policies carrying ALL these tags (key or key=value, comma-separated). Empty = no gate.")
}
