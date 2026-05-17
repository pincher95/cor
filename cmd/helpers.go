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
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/prompter"
)

// ec2NameTag returns the value of the "Name" tag in tags, or "" if absent or
// empty. Callers that want a display fallback (typically "-") apply it.
func ec2NameTag(tags []ec2types.Tag) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == "Name" {
			if v := aws.ToString(t.Value); v != "" {
				return v
			}
		}
	}
	return ""
}

func confirmDelete(p prompter.Client, logger *logging.Logger) (bool, error) {
	confirm, err := p.Confirm("Are you sure you want to proceed? (yes/no): ")
	if err != nil {
		logger.LogError("Error during user prompt", err, nil)
		return false, err
	}
	if confirm == nil || !*confirm {
		logger.LogInfo("Aborted.", nil)
		return false, nil
	}
	return true, nil
}

func normalizeFilterValue(value string) string {
	v := strings.TrimSpace(value)
	if v == "" || v == "*" {
		return ""
	}
	return v
}

func matchesFilterValue(value string, filter string) bool {
	if filter == "" {
		return true
	}
	if strings.ContainsAny(filter, "*?") {
		matched, err := path.Match(filter, value)
		if err == nil {
			return matched
		}
		clean := strings.NewReplacer("*", "", "?", "").Replace(filter)
		if clean == "" {
			return true
		}
		return strings.Contains(value, clean)
	}
	return strings.Contains(value, filter)
}

type tagFilter struct {
	key      string
	value    string
	hasValue bool
}

func parseTagFilters(input string) []tagFilter {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	filters := make([]tagFilter, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "=") {
			kv := strings.SplitN(part, "=", 2)
			key := strings.TrimSpace(kv[0])
			if key == "" {
				continue
			}
			value := strings.TrimSpace(kv[1])
			filters = append(filters, tagFilter{key: key, value: value, hasValue: true})
			continue
		}
		filters = append(filters, tagFilter{key: part})
	}
	if len(filters) == 0 {
		return nil
	}
	return filters
}

func tagsMatchFilters(tags map[string]string, filters []tagFilter) bool {
	if len(filters) == 0 {
		return true
	}
	if len(tags) == 0 {
		return false
	}
	for _, filter := range filters {
		value, ok := tags[filter.key]
		if !ok {
			return false
		}
		if filter.hasValue && value != filter.value {
			return false
		}
	}
	return true
}

// elbTagsToMap converts ELB tags (both classic and v2) to a map.
// This is a generic function that works with any tag type that has Key and Value fields.
func elbTagsToMap[T any](tags []T, keyExtractor, valueExtractor func(T) *string) map[string]string {
	if len(tags) == 0 {
		return map[string]string{}
	}
	tagMap := make(map[string]string, len(tags))
	for _, tag := range tags {
		key := aws.ToString(keyExtractor(tag))
		if key == "" {
			continue
		}
		tagMap[key] = aws.ToString(valueExtractor(tag))
	}
	return tagMap
}

// formatElbTags formats a tag map into a sorted, newline-separated string.
// Returns "-" if no tags are present.
func formatElbTags(tags map[string]string) string {
	if len(tags) == 0 {
		return "-"
	}
	values := make([]string, 0, len(tags))
	for key, value := range tags {
		if key == "" {
			continue
		}
		if value == "" {
			values = append(values, key)
		} else {
			values = append(values, key+"="+value)
		}
	}
	if len(values) == 0 {
		return "-"
	}
	sort.Strings(values)
	return strings.Join(values, "\n")
}

// awsNameCache is a goroutine-safe cache of AWS resource-name lookups
// keyed by VPC ID, subnet ID, and security-group ID. It eliminates
// duplicate Describe* calls when many items share the same VPC/subnet/SG.
//
// Construct with newAWSNameCache() — the three maps must be non-nil before
// use (writes to a nil map panic).
type awsNameCache struct {
	mu      sync.RWMutex
	vpcs    map[string]string
	subnets map[string]string
	sgs     map[string]string
}

// newAWSNameCache returns a ready-to-use awsNameCache with all three maps
// initialized to empty.
func newAWSNameCache() *awsNameCache {
	return &awsNameCache{
		vpcs:    map[string]string{},
		subnets: map[string]string{},
		sgs:     map[string]string{},
	}
}

// splitCSV splits a comma-separated filter value into trimmed, non-empty
// tokens, skipping "*" (the "no filter" sentinel). Returns nil when the
// input is empty or contains only skipped tokens.
func splitCSV(raw string) []string {
	raw = normalizeFilterValue(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || p == "*" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// getFlagString returns the string value for the given flag name, or "".
func getFlagString(extras *map[string]any, name string) string {
	if v, ok := (*extras)[name].(string); ok {
		return v
	}
	return ""
}

// mergeCSV unions CSV tokens from the named flags, preserving first-seen
// order and dropping duplicates.
func mergeCSV(extras *map[string]any, names ...string) []string {
	seen := make(map[string]struct{}, 8)
	out := make([]string, 0)
	for _, n := range names {
		for _, v := range splitCSV(getFlagString(extras, n)) {
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}
