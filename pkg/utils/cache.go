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

package utils

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// InstanceCache provides thread-safe caching for EC2 instance existence checks.
// This reduces duplicate API calls when checking if the same instance ID exists multiple times
// during a single command execution (e.g., when processing ELB targets).
type InstanceCache struct {
	cache map[string]bool
	mu    sync.RWMutex
}

// NewInstanceCache creates a new instance cache.
func NewInstanceCache() *InstanceCache {
	return &InstanceCache{
		cache: make(map[string]bool),
	}
}

// CheckExists checks if an EC2 instance exists, using the cache when possible.
// It makes an EC2 API call only on cache miss and stores the result for future lookups.
// Returns true if the instance exists, false otherwise.
func (c *InstanceCache) CheckExists(ctx context.Context, ec2Client *ec2.Client, instanceID string) (bool, error) {
	// Check cache first (read lock)
	c.mu.RLock()
	if exists, found := c.cache[instanceID]; found {
		c.mu.RUnlock()
		return exists, nil
	}
	c.mu.RUnlock()

	// Cache miss - query EC2 API
	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		// If instance not found, cache as non-existent
		c.mu.Lock()
		c.cache[instanceID] = false
		c.mu.Unlock()
		return false, err
	}

	// Instance exists if we have any reservations with instances
	exists := len(result.Reservations) > 0 && len(result.Reservations[0].Instances) > 0

	// Cache the result
	c.mu.Lock()
	c.cache[instanceID] = exists
	c.mu.Unlock()

	return exists, nil
}

// CheckExistsByFilter checks if an instance exists using EC2 filters, with caching.
// This is useful when checking by filter criteria rather than instance ID.
func (c *InstanceCache) CheckExistsByFilter(ctx context.Context, ec2Client *ec2.Client, cacheKey string, filters []ec2types.Filter) (bool, error) {
	// Check cache first (read lock)
	c.mu.RLock()
	if exists, found := c.cache[cacheKey]; found {
		c.mu.RUnlock()
		return exists, nil
	}
	c.mu.RUnlock()

	// Cache miss - query EC2 API
	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: filters,
	})
	if err != nil {
		return false, err
	}

	// Instance exists if we have any reservations with instances
	exists := len(result.Reservations) > 0 && len(result.Reservations[0].Instances) > 0

	// Cache the result
	c.mu.Lock()
	c.cache[cacheKey] = exists
	c.mu.Unlock()

	return exists, nil
}

// Clear removes all cached entries. Useful for testing or when you need to force fresh checks.
func (c *InstanceCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = make(map[string]bool)
}

// Size returns the number of cached entries. Useful for monitoring and debugging.
func (c *InstanceCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}
