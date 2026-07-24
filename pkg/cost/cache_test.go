/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cost

import (
	"os"
	"testing"
	"time"
)

func TestLoadCacheMissingFileIsNotAnError(t *testing.T) {
	isolateCacheHome(t)
	rt, fetchedAt, err := LoadCache("us-east-1")
	if err != nil {
		t.Fatalf("a cache miss must not be an error, got %v", err)
	}
	if rt != nil {
		t.Errorf("expected nil rate table on cache miss, got %+v", rt)
	}
	if !fetchedAt.IsZero() {
		t.Errorf("expected zero fetchedAt on cache miss, got %v", fetchedAt)
	}
}

func TestSaveCacheThenLoadCacheRoundTrips(t *testing.T) {
	isolateCacheHome(t)
	want := usEast1Rates()
	want.EBSVolume["gp3"] = 0.0777
	want.ClassicELBMonth = 99.5

	before := time.Now().Add(-time.Second)
	if err := SaveCache("eu-west-1", want); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	got, fetchedAt, err := LoadCache("eu-west-1")
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if got == nil {
		t.Fatal("expected a rate table after SaveCache")
	}
	almostEqual(t, got.EBSVolume["gp3"], 0.0777, "round-tripped gp3")
	almostEqual(t, got.ClassicELBMonth, 99.5, "round-tripped classic elb")
	almostEqual(t, got.EBSSnapshot, want.EBSSnapshot, "round-tripped snapshot")
	if len(got.S3StorageGB) != len(want.S3StorageGB) {
		t.Errorf("S3StorageGB map lost entries: got %d, want %d", len(got.S3StorageGB), len(want.S3StorageGB))
	}
	if fetchedAt.Before(before) {
		t.Errorf("fetchedAt %v should be at or after %v", fetchedAt, before)
	}
}

func TestSaveCacheIsAtomicAndLeavesNoTempFile(t *testing.T) {
	isolateCacheHome(t)
	if err := SaveCache("us-east-2", usEast1Rates()); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	path, err := cacheFilePath("us-east-2")
	if err != nil {
		t.Fatalf("cacheFilePath: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected cache file at %s: %v", path, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file %s.tmp should have been renamed away", path)
	}
}

func TestSaveCacheOverwritesPreviousTable(t *testing.T) {
	isolateCacheHome(t)
	first := usEast1Rates()
	first.ECRGB = 1.5
	if err := SaveCache("us-east-1", first); err != nil {
		t.Fatalf("first SaveCache: %v", err)
	}
	second := usEast1Rates()
	second.ECRGB = 2.5
	if err := SaveCache("us-east-1", second); err != nil {
		t.Fatalf("second SaveCache: %v", err)
	}
	got, _, err := LoadCache("us-east-1")
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	almostEqual(t, got.ECRGB, 2.5, "overwritten rate")
}

func TestLoadCacheRejectsCorruptAndEmptyPayloads(t *testing.T) {
	t.Run("malformed json", func(t *testing.T) {
		isolateCacheHome(t)
		writeRawCache(t, "us-east-1", "{not json")
		if _, _, err := LoadCache("us-east-1"); err == nil {
			t.Error("expected an error for malformed cache JSON")
		}
	})

	t.Run("null rates", func(t *testing.T) {
		isolateCacheHome(t)
		writeRawCache(t, "us-east-1", `{"region":"us-east-1","rates":null}`)
		if _, _, err := LoadCache("us-east-1"); err == nil {
			t.Error("expected an error when the cache carries no rates")
		}
	})
}

func TestNewPrefersCacheOverBundledDefaults(t *testing.T) {
	isolateCacheHome(t)
	cached := usEast1Rates()
	cached.EBSVolume["gp3"] = 0.5
	if err := SaveCache("eu-central-1", cached); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	p := New("eu-central-1")
	almostEqual(t, p.EBSVolumeGB("gp3"), 0.5, "cached gp3 wins over bundled default")

	// A different region must not read eu-central-1's cache file.
	other := New("us-east-1")
	almostEqual(t, other.EBSVolumeGB("gp3"), 0.08, "unrelated region keeps bundled default")
}

func TestNewFallsBackToDefaultsWhenCacheIsCorrupt(t *testing.T) {
	isolateCacheHome(t)
	writeRawCache(t, "us-east-1", "{truncated")
	p := New("us-east-1")
	almostEqual(t, p.EBSVolumeGB("gp3"), 0.08, "corrupt cache must not break pricing")
}

func TestCacheStatusForReportsPresence(t *testing.T) {
	isolateCacheHome(t)
	status, err := CacheStatusFor("us-east-1")
	if err != nil {
		t.Fatalf("CacheStatusFor before save: %v", err)
	}
	if status.Present {
		t.Error("expected Present=false before any cache is written")
	}
	if status.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", status.Region)
	}

	if err := SaveCache("us-east-1", usEast1Rates()); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	status, err = CacheStatusFor("us-east-1")
	if err != nil {
		t.Fatalf("CacheStatusFor after save: %v", err)
	}
	if !status.Present {
		t.Error("expected Present=true after writing a cache")
	}
	if status.FetchedAt.IsZero() {
		t.Error("expected a non-zero FetchedAt after writing a cache")
	}
}

func writeRawCache(t *testing.T, region, contents string) {
	t.Helper()
	path, err := cacheFilePath(region)
	if err != nil {
		t.Fatalf("cacheFilePath: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing raw cache: %v", err)
	}
}
