/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
	"github.com/pincher95/cor/pkg/handlers/aws/awstest"
)

// itemFromJSON decodes a Pricing API PriceList entry the same way
// forEachPrice does, so these tests exercise the real decode path.
func itemFromJSON(t *testing.T, raw string) priceItem {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	return priceItem{raw: m}
}

func TestContains(t *testing.T) {
	cases := []struct {
		haystack, needle string
		want             bool
	}{
		{"AmazonS3-Storage", "Storage", true},
		{"AmazonS3-Storage", "storage", false},
		{"prefix", "", true},
		{"", "", true},
		{"short", "longer-needle", false},
		{"ChargedBackupUsage", "ChargedBackupUsage", true},
		{"RDS:ChargedBackupUsage", "Backup", true},
		{"nope", "Backup", false},
	}
	for _, tc := range cases {
		if got := contains(tc.haystack, tc.needle); got != tc.want {
			t.Errorf("contains(%q, %q) = %v, want %v", tc.haystack, tc.needle, got, tc.want)
		}
	}
}

func TestCloneStringUSDProducesIndependentCopy(t *testing.T) {
	original := map[string]USD{"gp3": 0.08, "gp2": 0.10}
	clone := cloneStringUSD(original)

	if len(clone) != len(original) {
		t.Fatalf("clone has %d entries, want %d", len(clone), len(original))
	}
	almostEqual(t, clone["gp3"], 0.08, "cloned gp3")

	clone["gp3"] = 9.99
	clone["new"] = 1.0
	almostEqual(t, original["gp3"], 0.08, "original must be untouched")
	if _, ok := original["new"]; ok {
		t.Error("writing to the clone leaked a key into the original")
	}
}

func TestCloneStringUSDOnEmptyInput(t *testing.T) {
	clone := cloneStringUSD(nil)
	if clone == nil {
		t.Fatal("clone of a nil map must be non-nil so callers can write to it")
	}
	if len(clone) != 0 {
		t.Errorf("expected an empty clone, got %d entries", len(clone))
	}
	clone["x"] = 1 // must not panic
}

func TestEqFilterBuildsTermMatch(t *testing.T) {
	f := eqFilter("regionCode", "eu-west-1")
	if f.Type != pricingtypes.FilterTypeTermMatch {
		t.Errorf("Type = %v, want TERM_MATCH", f.Type)
	}
	if aws.ToString(f.Field) != "regionCode" {
		t.Errorf("Field = %q, want regionCode", aws.ToString(f.Field))
	}
	if aws.ToString(f.Value) != "eu-west-1" {
		t.Errorf("Value = %q, want eu-west-1", aws.ToString(f.Value))
	}
}

func TestAttrString(t *testing.T) {
	item := itemFromJSON(t, `{
		"product": {"attributes": {"volumeType": "Standard", "instanceType": "m5.large"}}
	}`)

	if v, ok := item.attrString("volumeType"); !ok || v != "Standard" {
		t.Errorf("attrString(volumeType) = (%q, %v), want (Standard, true)", v, ok)
	}
	if v, ok := item.attrString("instanceType"); !ok || v != "m5.large" {
		t.Errorf("attrString(instanceType) = (%q, %v), want (m5.large, true)", v, ok)
	}
	if _, ok := item.attrString("missing"); ok {
		t.Error("attrString for an absent attribute should report ok=false")
	}
}

func TestAttrStringOnMalformedShapes(t *testing.T) {
	for name, raw := range map[string]string{
		"empty object":         `{}`,
		"no attributes":        `{"product": {}}`,
		"product is a string":  `{"product": "nope"}`,
		"attributes is a list": `{"product": {"attributes": []}}`,
		"value is not string":  `{"product": {"attributes": {"volumeType": 42}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			item := itemFromJSON(t, raw)
			if _, ok := item.attrString("volumeType"); ok {
				t.Error("expected ok=false for a malformed product shape")
			}
		})
	}
}

func TestOnDemandUSDPerUnitReadsNestedPrice(t *testing.T) {
	item := itemFromJSON(t, `{
		"terms": {"OnDemand": {"TERM1": {"priceDimensions": {"DIM1": {
			"pricePerUnit": {"USD": "0.0230000000"}
		}}}}}
	}`)
	rate, ok := item.onDemandUSDPerUnit()
	if !ok {
		t.Fatal("expected to find a price")
	}
	almostEqual(t, rate, 0.023, "parsed price")

	// onDemandUSDPerHour is the same lookup, just a naming alias.
	hourly, ok := item.onDemandUSDPerHour()
	if !ok {
		t.Fatal("expected onDemandUSDPerHour to find the same price")
	}
	almostEqual(t, hourly, 0.023, "hourly alias")
}

func TestOnDemandUSDPerUnitSkipsZeroAndUnparseablePrices(t *testing.T) {
	for name, raw := range map[string]string{
		"no terms":        `{}`,
		"empty on demand": `{"terms": {"OnDemand": {}}}`,
		"zero price": `{"terms": {"OnDemand": {"T": {"priceDimensions": {"D": {
			"pricePerUnit": {"USD": "0.0000000000"}}}}}}}`,
		"unparseable price": `{"terms": {"OnDemand": {"T": {"priceDimensions": {"D": {
			"pricePerUnit": {"USD": "not-a-number"}}}}}}}`,
		"missing usd key": `{"terms": {"OnDemand": {"T": {"priceDimensions": {"D": {
			"pricePerUnit": {"CNY": "1.0"}}}}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			item := itemFromJSON(t, raw)
			if rate, ok := item.onDemandUSDPerUnit(); ok || rate != 0 {
				t.Errorf("expected (0, false), got (%v, %v)", float64(rate), ok)
			}
		})
	}
}

func TestS3FirstTierUSDPicksTheZeroBeginRangeTier(t *testing.T) {
	// Two tiers in one term. Map iteration order is random, so only the
	// beginRange=="0" filter makes this deterministic — that is the point of
	// s3FirstTierUSD existing alongside onDemandUSDPerUnit.
	item := itemFromJSON(t, `{
		"terms": {"OnDemand": {"TERM1": {"priceDimensions": {
			"TIER1": {"beginRange": "0",     "pricePerUnit": {"USD": "0.0230000000"}},
			"TIER2": {"beginRange": "51200", "pricePerUnit": {"USD": "0.0220000000"}},
			"TIER3": {"beginRange": "512000","pricePerUnit": {"USD": "0.0210000000"}}
		}}}}
	}`)
	rate, ok := s3FirstTierUSD(item)
	if !ok {
		t.Fatal("expected to find the first tier")
	}
	almostEqual(t, rate, 0.023, "first-tier rate")
}

func TestS3FirstTierUSDWithoutAZeroTier(t *testing.T) {
	item := itemFromJSON(t, `{
		"terms": {"OnDemand": {"TERM1": {"priceDimensions": {
			"TIER2": {"beginRange": "51200", "pricePerUnit": {"USD": "0.0220000000"}}
		}}}}
	}`)
	if rate, ok := s3FirstTierUSD(item); ok || rate != 0 {
		t.Errorf("expected (0, false) when no tier begins at 0, got (%v, %v)", float64(rate), ok)
	}
}

// The refresh path writes rates keyed by CloudWatch StorageType, and
// cor s3buckets reads them back by the same key. If the mapping ever names a
// class the bundled table doesn't know, refreshed prices would silently land
// in a key nothing reads.
func TestS3VolumeTypeMappingTargetsExistInRateTable(t *testing.T) {
	rates := defaultRates["us-east-1"].S3StorageGB
	for volumeType, cwClass := range s3VolumeTypeToCWClass {
		if _, ok := rates[cwClass]; !ok {
			t.Errorf("volumeType %q maps to CloudWatch class %q, which has no entry in the rate table",
				volumeType, cwClass)
		}
	}
}

func TestFetchEBSVolumesOverlaysFetchedRatesOntoTable(t *testing.T) {
	cfg := awstest.Config(t, nil, awstest.Stubs{
		"GetProducts": &pricing.GetProductsOutput{
			PriceList: []string{
				`{"product": {"attributes": {"volumeApiName": "gp3"}},
				  "terms": {"OnDemand": {"T": {"priceDimensions": {"D": {
				      "pricePerUnit": {"USD": "0.0640000000"}}}}}}}`,
				`{"product": {"attributes": {"volumeApiName": "io2"}},
				  "terms": {"OnDemand": {"T": {"priceDimensions": {"D": {
				      "pricePerUnit": {"USD": "0.1000000000"}}}}}}}`,
				// No volumeApiName — must be ignored, not crash.
				`{"product": {"attributes": {}},
				  "terms": {"OnDemand": {"T": {"priceDimensions": {"D": {
				      "pricePerUnit": {"USD": "9.9900000000"}}}}}}}`,
				// Zero price — must be ignored so a real rate is not clobbered.
				`{"product": {"attributes": {"volumeApiName": "st1"}},
				  "terms": {"OnDemand": {"T": {"priceDimensions": {"D": {
				      "pricePerUnit": {"USD": "0.0000000000"}}}}}}}`,
				// Not valid JSON — forEachPrice skips it rather than aborting.
				`{"product": broken`,
			},
		},
	})

	rt := usEast1Rates()
	rt.EBSVolume = cloneStringUSD(rt.EBSVolume)
	if err := fetchEBSVolumes(context.Background(), pricing.NewFromConfig(cfg), "eu-west-1", rt); err != nil {
		t.Fatalf("fetchEBSVolumes: %v", err)
	}

	almostEqual(t, rt.EBSVolume["gp3"], 0.064, "gp3 overwritten by fetched rate")
	almostEqual(t, rt.EBSVolume["io2"], 0.100, "io2 overwritten by fetched rate")
	almostEqual(t, rt.EBSVolume["st1"], 0.045, "st1 keeps its seeded rate when the fetched price is zero")
	almostEqual(t, rt.EBSVolume["gp2"], 0.10, "untouched types keep their seeded rate")
	if _, ok := rt.EBSVolume[""]; ok {
		t.Error("an item without volumeApiName must not create an empty-string key")
	}
}

func TestFetchEBSVolumesPropagatesAPIError(t *testing.T) {
	sentinel := errors.New("pricing api unavailable")
	cfg := awstest.Config(t, nil, awstest.Stubs{
		"GetProducts": func(context.Context, any) (any, error) { return nil, sentinel },
	})
	rt := usEast1Rates()
	err := fetchEBSVolumes(context.Background(), pricing.NewFromConfig(cfg), "us-east-1", rt)
	if err == nil {
		t.Fatal("expected the API error to propagate")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the sentinel in the error chain, got %v", err)
	}
}

// cmd/pricing.go always calls SaveCache on whatever RefreshRegion returns, so
// RefreshRegion must return a usable table even when a fetcher fails.
func TestRefreshRegionReturnsSeededTableEvenOnError(t *testing.T) {
	cfg := awstest.Config(t, nil, awstest.Stubs{
		"GetProducts": func(context.Context, any) (any, error) {
			return nil, errors.New("pricing api unavailable")
		},
	})

	rt, err := RefreshRegion(context.Background(), cfg, "eu-west-1")
	if err == nil {
		t.Fatal("expected an error when the Pricing API fails")
	}
	if rt == nil {
		t.Fatal("RefreshRegion must never return a nil table; cmd/pricing.go caches it unconditionally")
	}
	almostEqual(t, rt.EBSVolume["gp3"], 0.08, "seeded gp3 survives a failed refresh")
	almostEqual(t, rt.EBSSnapshot, 0.05, "seeded snapshot rate survives a failed refresh")
	if len(rt.S3StorageGB) == 0 {
		t.Error("seeded S3 storage classes should survive a failed refresh")
	}
}

func TestRefreshRegionSeedMapsAreIndependentOfDefaults(t *testing.T) {
	cfg := awstest.Config(t, nil, awstest.Stubs{
		"GetProducts": func(context.Context, any) (any, error) {
			return nil, errors.New("stop early")
		},
	})
	rt, _ := RefreshRegion(context.Background(), cfg, "eu-west-1")
	if rt == nil {
		t.Fatal("expected a table")
	}
	rt.EBSVolume["gp3"] = 42
	rt.S3StorageGB["StandardStorage"] = 42
	almostEqual(t, defaultRates["us-east-1"].EBSVolume["gp3"], 0.08, "bundled defaults must not be mutated")
	almostEqual(t, defaultRates["us-east-1"].S3StorageGB["StandardStorage"], 0.023, "bundled defaults must not be mutated")
}

func TestS3VolumeTypeMappingIsInjective(t *testing.T) {
	seen := make(map[string]string, len(s3VolumeTypeToCWClass))
	for volumeType, cwClass := range s3VolumeTypeToCWClass {
		if prev, ok := seen[cwClass]; ok {
			t.Errorf("CloudWatch class %q is mapped from both %q and %q; one would overwrite the other",
				cwClass, prev, volumeType)
		}
		seen[cwClass] = volumeType
	}
}
