package utils

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Checks if string in slice returns index and bool
func SliceContainsWithIndex[T comparable](slice []T, search T) (int, bool) {
	for i, v := range slice {
		fmt.Println("Name: ", v)
		fmt.Println("Element: ", v)
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
		tagMap[*tag.Key] = tag
	}
	return tagMap
}
