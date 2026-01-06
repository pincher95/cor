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

package utils

import (
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Checks if string in slice returns index and bool
func SliceContainsWithIndex[T comparable](slice []T, search T) (int, bool) {
	for i, v := range slice {
		if v == search {
			return i, true
		}
	}

	return -1, false
}

// RemoveFromSlice removes elements from slice a based on indices in slice b
func RemoveFromSlice[T comparable](sliceA, sliceB []T) []T {
	// Create a map to store elements from slice b
	indexMap := make(map[T]bool)
	for _, index := range sliceB {
		indexMap[index] = true
	}

	// Create a new slice to store elements from slice a
	var result []T
	for _, val := range sliceA {
		// If the element is not present in the map, add it to the result slice
		if !indexMap[val] {
			result = append(result, val)
		}
	}
	return result
}

func SliceChunkBy[T any](items []T, chunkSize int) (chunks [][]T) {
	// Guard against invalid chunk sizes to avoid panics/div-by-zero.
	// When chunkSize <= 0, return the whole slice as a single chunk.
	if chunkSize <= 0 {
		if items == nil {
			return nil
		}
		return [][]T{items}
	}
	var _chunks = make([][]T, 0, (len(items)/chunkSize)+1)
	for chunkSize < len(items) {
		items, _chunks = items[chunkSize:], append(_chunks, items[0:chunkSize:chunkSize])
	}
	return append(_chunks, items)
}

// Converting the slice of tags into a map
func TagsToMap(tags []types.Tag) map[string]types.Tag {
	tagMap := make(map[string]types.Tag)
	for _, tag := range tags {
		if tag.Key == nil {
			continue
		}
		tagMap[*tag.Key] = tag
	}
	return tagMap
}
