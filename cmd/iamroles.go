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
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	handlers "github.com/pincher95/cor/pkg/handlers/aws"
	"github.com/pincher95/cor/pkg/handlers/flags"
	"github.com/spf13/cobra"
)

type orphanIAMRole struct {
	name, arn, path  string
	lastUsed         string
	reason           string
	attachedPolicies []string
	inlinePolicies   []string
	instanceProfiles []string
}

var iamRolesCmd = &cobra.Command{
	Use:   "iamroles",
	Short: "List and optionally delete unused customer-managed IAM roles",
	Long: `List IAM roles unused within --max-unused-days (or never used and older than
that window), excluding AWS service-linked roles, and optionally delete them
together with their inline/attached policies and instance profiles.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runResourceCommand(cmd, CommandSetup{
			AdditionalFlags: []flags.Flag{
				{Name: "filter-by-name", Type: "string"},
				{Name: "path-prefix", Type: "string"},
				{Name: "max-unused-days", Type: "int"},
				{Name: "include-empty", Type: "bool"},
				{Name: "require-tag", Type: "string"},
			},
			BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
				return &handlers.AWSClientImpl{IAM: iam.NewFromConfig(*cfg)}
			},
		}, (*AWSCommand).executeIAMRoles)
	},
}

func (a *AWSCommand) executeIAMRoles(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
	filterByName := normalizeFilterValue(getFlagString(extras, "filter-by-name"))
	pathPrefix := getFlagString(extras, "path-prefix")
	maxUnusedDays, _ := (*extras)["max-unused-days"].(int)
	if maxUnusedDays <= 0 {
		maxUnusedDays = 90
	}
	includeEmpty, _ := (*extras)["include-empty"].(bool)
	requireTag := parseTagFilters(getFlagString(extras, "require-tag"))

	return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[iamtypes.Role, orphanIAMRole]{
		Headers:       []string{"Role", "Path", "Last Used", "Reason"},
		ResourceLabel: "IAM roles",
		HideIndex:     true,
		List: func(ctx context.Context, emit func(iamtypes.Role) error) error {
			in := &iam.ListRolesInput{}
			if pathPrefix != "" {
				in.PathPrefix = aws.String(pathPrefix)
			}
			p := iam.NewListRolesPaginator(a.AWSClient.IAM, in)
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, role := range page.Roles {
					if err := emit(role); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, role iamtypes.Role) (*orphanIAMRole, error) {
			// Service-linked roles are AWS-owned and must not be touched.
			if strings.HasPrefix(aws.ToString(role.Path), "/aws-service-role/") {
				return nil, nil
			}
			name := aws.ToString(role.RoleName)
			if filterByName != "" && !matchesFilterValue(name, filterByName) {
				return nil, nil
			}

			// ListRoles does not populate RoleLastUsed; GetRole does.
			got, err := a.AWSClient.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: role.RoleName})
			if err != nil {
				a.Logger.LogError("GetRole failed; skipping role", err, map[string]any{"role": name})
				return nil, nil
			}
			full := got.Role
			ageDays := int(time.Since(aws.ToTime(full.CreateDate)).Hours() / 24)

			lastUsedLabel := "never"
			lastUsedDays := -1
			if full.RoleLastUsed != nil && full.RoleLastUsed.LastUsedDate != nil {
				lud := *full.RoleLastUsed.LastUsedDate
				lastUsedDays = int(time.Since(lud).Hours() / 24)
				lastUsedLabel = lud.Format("2006-01-02")
			}

			var reason string
			switch {
			case lastUsedDays < 0 && ageDays >= maxUnusedDays:
				reason = fmt.Sprintf("Never used (%d days old)", ageDays)
			case lastUsedDays >= maxUnusedDays:
				reason = fmt.Sprintf("Unused %d days", lastUsedDays)
			}

			attached, inline, profiles, err := a.iamRoleAttachments(ctx, name)
			if err != nil {
				a.Logger.LogError("listing role attachments failed; skipping", err, map[string]any{"role": name})
				return nil, nil
			}
			if reason == "" && includeEmpty && len(attached) == 0 && len(inline) == 0 && len(profiles) == 0 {
				reason = "No policies attached"
			}
			if reason == "" {
				return nil, nil
			}
			if len(requireTag) > 0 && !tagsMatchFilters(iamTagsToMap(full.Tags), requireTag) {
				return nil, nil
			}

			return &orphanIAMRole{
				name:             name,
				arn:              aws.ToString(full.Arn),
				path:             aws.ToString(full.Path),
				lastUsed:         lastUsedLabel,
				reason:           reason,
				attachedPolicies: attached,
				inlinePolicies:   inline,
				instanceProfiles: profiles,
			}, nil
		},
		ToRow: func(r orphanIAMRole) []any {
			return []any{r.name, r.path, r.lastUsed, r.reason}
		},
		Delete:            a.deleteIAMRole,
		DeleteConcurrency: 3,
		DedupKey:          func(r orphanIAMRole) string { return r.arn },
	})
}

func (a *AWSCommand) iamRoleAttachments(ctx context.Context, roleName string) (attached, inline, profiles []string, err error) {
	ap := iam.NewListAttachedRolePoliciesPaginator(a.AWSClient.IAM, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(roleName)})
	for ap.HasMorePages() {
		page, e := ap.NextPage(ctx)
		if e != nil {
			return nil, nil, nil, e
		}
		for _, p := range page.AttachedPolicies {
			attached = append(attached, aws.ToString(p.PolicyArn))
		}
	}
	ip := iam.NewListRolePoliciesPaginator(a.AWSClient.IAM, &iam.ListRolePoliciesInput{RoleName: aws.String(roleName)})
	for ip.HasMorePages() {
		page, e := ip.NextPage(ctx)
		if e != nil {
			return nil, nil, nil, e
		}
		inline = append(inline, page.PolicyNames...)
	}
	pp := iam.NewListInstanceProfilesForRolePaginator(a.AWSClient.IAM, &iam.ListInstanceProfilesForRoleInput{RoleName: aws.String(roleName)})
	for pp.HasMorePages() {
		page, e := pp.NextPage(ctx)
		if e != nil {
			return nil, nil, nil, e
		}
		for _, prof := range page.InstanceProfiles {
			profiles = append(profiles, aws.ToString(prof.InstanceProfileName))
		}
	}
	return attached, inline, profiles, nil
}

// deleteIAMRole enforces the mandatory teardown order before DeleteRole: detach
// managed policies, delete inline policies, remove the role from instance
// profiles, then delete the role.
func (a *AWSCommand) deleteIAMRole(ctx context.Context, r orphanIAMRole) error {
	for _, arn := range r.attachedPolicies {
		if _, err := a.AWSClient.IAM.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
			RoleName:  aws.String(r.name),
			PolicyArn: aws.String(arn),
		}); err != nil {
			return fmt.Errorf("detach %s: %w", arn, err)
		}
	}
	for _, name := range r.inlinePolicies {
		if _, err := a.AWSClient.IAM.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
			RoleName:   aws.String(r.name),
			PolicyName: aws.String(name),
		}); err != nil {
			return fmt.Errorf("delete inline %s: %w", name, err)
		}
	}
	for _, prof := range r.instanceProfiles {
		if _, err := a.AWSClient.IAM.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
			InstanceProfileName: aws.String(prof),
			RoleName:            aws.String(r.name),
		}); err != nil {
			return fmt.Errorf("remove from profile %s: %w", prof, err)
		}
	}
	a.Logger.LogInfo("Deleting IAM role", map[string]any{"role": r.name, "reason": r.reason})
	_, err := a.AWSClient.IAM.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(r.name)})
	return err
}

func init() {
	iamRolesCmd.Flags().String("filter-by-name", "", "Filter roles by name substring/glob (empty = no filter).")
	iamRolesCmd.Flags().String("path-prefix", "", "Only list roles under this IAM path prefix (e.g. /application/).")
	iamRolesCmd.Flags().Int("max-unused-days", 90, "Flag roles unused for at least this many days (never-used roles must also be this old).")
	iamRolesCmd.Flags().Bool("include-empty", false, "Also flag roles with zero attached/inline policies and no instance profile.")
	iamRolesCmd.Flags().String("require-tag", "", "Safety gate: only consider roles carrying ALL these tags (key or key=value, comma-separated). Empty = no gate.")
}
