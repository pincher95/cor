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
	"strings"

	"github.com/pincher95/cor/pkg/handlers/logging"
	"github.com/pincher95/cor/pkg/handlers/prompter"
)

func confirmDelete(p prompter.Client, logger *logging.Logger) (bool, error) {
	confirm, err := p.Confirm("Are you sure you want to proceed? (yes/no): ")
	if err != nil {
		logger.LogError("Error during user prompt", err, nil, false)
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
