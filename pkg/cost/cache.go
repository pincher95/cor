/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cost

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// cacheEnvelope wraps a rateTable with metadata so users can tell how
// fresh the cache is.
type cacheEnvelope struct {
	Region    string     `json:"region"`
	FetchedAt time.Time  `json:"fetched_at"`
	Rates     *rateTable `json:"rates"`
}

// cacheDir returns ~/.cor/prices, creating it on demand.
func cacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".cor", "prices")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// cacheFilePath returns the cache file for the given region.
func cacheFilePath(region string) (string, error) {
	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, region+".json"), nil
}

// LoadCache returns the cached rate table for the region, when it exists.
// Returns (nil, time.Time{}, nil) when the file is absent — a cache miss
// is not an error. Other I/O or decode failures return a non-nil error.
func LoadCache(region string) (*rateTable, time.Time, error) {
	path, err := cacheFilePath(region)
	if err != nil {
		return nil, time.Time{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, time.Time{}, nil
		}
		return nil, time.Time{}, err
	}
	var env cacheEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, time.Time{}, err
	}
	if env.Rates == nil {
		return nil, env.FetchedAt, errors.New("cache file present but rates are empty")
	}
	return env.Rates, env.FetchedAt, nil
}

// SaveCache writes the rate table for the given region atomically (write
// to a sibling tmp file, then rename) so concurrent reads never see a
// truncated file.
func SaveCache(region string, r *rateTable) error {
	path, err := cacheFilePath(region)
	if err != nil {
		return err
	}
	env := cacheEnvelope{
		Region:    region,
		FetchedAt: time.Now().UTC(),
		Rates:     r,
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// CacheStatus describes whether a region has a cache hit + when.
type CacheStatus struct {
	Region    string
	Present   bool
	FetchedAt time.Time
}

// CacheStatusFor returns the cache freshness for the region. The first
// return value's Present field reports whether a cache file exists.
func CacheStatusFor(region string) (CacheStatus, error) {
	path, err := cacheFilePath(region)
	if err != nil {
		return CacheStatus{Region: region}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CacheStatus{Region: region}, nil
		}
		return CacheStatus{Region: region}, err
	}
	return CacheStatus{Region: region, Present: true, FetchedAt: info.ModTime()}, nil
}
