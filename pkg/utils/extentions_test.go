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
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestSliceContainsWithIndex(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		i, ok := SliceContainsWithIndex([]string{"a", "b", "c"}, "b")
		if !ok {
			t.Fatalf("expected ok=true")
		}
		if i != 1 {
			t.Fatalf("expected index=1, got %d", i)
		}
	})

	t.Run("not found", func(t *testing.T) {
		i, ok := SliceContainsWithIndex([]int{1, 2, 3}, 4)
		if ok {
			t.Fatalf("expected ok=false")
		}
		if i != -1 {
			t.Fatalf("expected index=-1, got %d", i)
		}
	})
}

func TestRemoveFromSlice(t *testing.T) {
	got := RemoveFromSlice([]string{"a", "b", "c", "b"}, []string{"b"})
	want := []string{"a", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected result.\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestSliceChunkBy(t *testing.T) {
	t.Run("chunks", func(t *testing.T) {
		got := SliceChunkBy([]int{1, 2, 3, 4, 5}, 2)
		want := [][]int{{1, 2}, {3, 4}, {5}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected chunks.\nwant: %#v\ngot:  %#v", want, got)
		}
	})

	t.Run("invalid chunk size", func(t *testing.T) {
		got := SliceChunkBy([]int{1, 2, 3}, 0)
		want := [][]int{{1, 2, 3}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected chunks.\nwant: %#v\ngot:  %#v", want, got)
		}
	})

	t.Run("nil items", func(t *testing.T) {
		var items []int
		got := SliceChunkBy(items, 0)
		if got != nil {
			t.Fatalf("expected nil, got %#v", got)
		}
	})
}

func TestTagsToMap(t *testing.T) {
	tags := []types.Tag{
		{Key: aws.String("Name"), Value: aws.String("v1")},
		{Key: nil, Value: aws.String("ignored")},
		{Key: aws.String("Name"), Value: aws.String("v2")}, // overwrite
	}
	m := TagsToMap(tags)
	if len(m) != 1 {
		t.Fatalf("expected 1 tag, got %d", len(m))
	}
	if aws.ToString(m["Name"].Value) != "v2" {
		t.Fatalf("expected Name=v2, got %q", aws.ToString(m["Name"].Value))
	}
}
