/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cost

import (
	"sort"
	"testing"
)

// isolateCacheHome points os.UserHomeDir at a temp dir so New() cannot pick up
// the developer's real ~/.cor/prices cache and skew these assertions.
func isolateCacheHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestNewUsesBundledUSEast1Rates(t *testing.T) {
	isolateCacheHome(t)
	p := New("us-east-1")
	if p.Region() != "us-east-1" {
		t.Errorf("Region() = %q, want us-east-1", p.Region())
	}
	almostEqual(t, p.EBSVolumeGB("gp3"), 0.08, "gp3")
	almostEqual(t, p.EBSSnapshotGB(), 0.05, "snapshot")
}

func TestNewUnknownRegionSilentlyFallsBackToUSEast1(t *testing.T) {
	isolateCacheHome(t)
	p := New("ap-south-1")
	// Region() reports what was asked for even though the rates are
	// us-east-1's — this fallback is silent by design, so pin the behavior.
	if p.Region() != "ap-south-1" {
		t.Errorf("Region() = %q, want ap-south-1", p.Region())
	}
	almostEqual(t, p.EBSVolumeGB("gp3"), defaultRates["us-east-1"].EBSVolume["gp3"], "fallback gp3")
	almostEqual(t, p.ClassicELBMonth(), defaultRates["us-east-1"].ClassicELBMonth, "fallback classic elb")
}

func TestEBSVolumeGBFallsBackToGP3ForUnknownType(t *testing.T) {
	isolateCacheHome(t)
	p := New("us-east-1")
	almostEqual(t, p.EBSVolumeGB("gp2"), 0.10, "gp2")
	almostEqual(t, p.EBSVolumeGB("io2"), 0.125, "io2")
	almostEqual(t, p.EBSVolumeGB("sc1"), 0.015, "sc1")
	almostEqual(t, p.EBSVolumeGB("does-not-exist"), p.EBSVolumeGB("gp3"), "unknown type")
}

func TestUnknownLookupsReturnZeroRatherThanGuessing(t *testing.T) {
	isolateCacheHome(t)
	p := New("us-east-1")
	cases := map[string]USD{
		"RDSInstanceClassMonth": p.RDSInstanceClassMonth("db.nope.xlarge"),
		"EC2InstanceMonth":      p.EC2InstanceMonth("zz.mega"),
		"ElastiCacheNodeMonth":  p.ElastiCacheNodeMonth("cache.nope.large"),
		"OpenSearchNodeMonth":   p.OpenSearchNodeMonth("nope.search"),
		"S3StorageGB":           p.S3StorageGB("NotAStorageClass"),
		"BedrockMUHour":         p.BedrockProvisionedMUHour("some.unmodeled-model-v9"),
	}
	for name, got := range cases {
		if got != 0 {
			t.Errorf("%s for an unknown key = %v, want 0 (renders as em dash)", name, float64(got))
		}
	}
}

func TestKnownInstanceAndNodeRates(t *testing.T) {
	isolateCacheHome(t)
	p := New("us-east-1")
	almostEqual(t, p.RDSInstanceClassMonth("db.t3.micro"), 12.41, "db.t3.micro")
	almostEqual(t, p.EC2InstanceMonth("m5.large"), 70.08, "m5.large")
	almostEqual(t, p.ElastiCacheNodeMonth("cache.r6g.xlarge"), 217.18, "cache.r6g.xlarge")
	almostEqual(t, p.OpenSearchNodeMonth("t3.small.search"), 26.30, "t3.small.search")
	almostEqual(t, p.S3StorageGB("StandardStorage"), 0.023, "StandardStorage")
	almostEqual(t, p.S3StorageGB("DeepArchiveStorage"), 0.00099, "DeepArchiveStorage")
}

func TestDynamoDBCapacityRatesProjectHourlyToMonthly(t *testing.T) {
	isolateCacheHome(t)
	p := New("us-east-1")
	almostEqual(t, p.DynamoDBWCUMonth(), 0.00065*HoursPerMonth, "WCU monthly")
	almostEqual(t, p.DynamoDBRCUMonth(), 0.00013*HoursPerMonth, "RCU monthly")
}

func TestBedrockProvisionedMUHourMatchesModelIDSubstring(t *testing.T) {
	isolateCacheHome(t)
	p := New("us-east-1")
	cases := []struct {
		modelID string
		want    USD
	}{
		{"anthropic.claude-3-5-sonnet-20240620-v1:0", 29.0},
		{"anthropic.claude-3-sonnet-20240229-v1:0", 25.0},
		{"anthropic.claude-3-haiku-20240307-v1:0", 5.0},
		{"anthropic.claude-3-5-haiku-20241022-v1:0", 6.5},
		{"anthropic.claude-3-opus-20240229-v1:0", 80.0},
		{"meta.llama3-70b-instruct-v1:0", 8.0},
		{"amazon.titan-embed-text-v1", 0.8},
		{"cohere.command-r-plus-v1:0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.modelID, func(t *testing.T) {
			almostEqual(t, p.BedrockProvisionedMUHour(tc.modelID), tc.want, tc.modelID)
		})
	}
}

func TestBedrockMatchIsCaseInsensitive(t *testing.T) {
	isolateCacheHome(t)
	p := New("us-east-1")
	almostEqual(t, p.BedrockProvisionedMUHour("ANTHROPIC.CLAUDE-3-OPUS-V1"), 80.0, "upper-case model id")
}

func TestS3StorageClassesIsSortedAndCoversRateTable(t *testing.T) {
	isolateCacheHome(t)
	p := New("us-east-1")
	classes := p.S3StorageClasses()
	if len(classes) != len(defaultRates["us-east-1"].S3StorageGB) {
		t.Fatalf("S3StorageClasses() returned %d classes, want %d",
			len(classes), len(defaultRates["us-east-1"].S3StorageGB))
	}
	if !sort.StringsAreSorted(classes) {
		t.Errorf("S3StorageClasses() must be sorted for stable fan-out, got %v", classes)
	}
	for _, c := range classes {
		if p.S3StorageGB(c) <= 0 {
			t.Errorf("class %q is advertised but has no positive rate", c)
		}
	}
}

func TestEntriesIsStableAcrossCalls(t *testing.T) {
	isolateCacheHome(t)
	p := New("us-east-1")
	first, second := p.Entries(), p.Entries()
	if len(first) != len(second) {
		t.Fatalf("Entries() length changed between calls: %d then %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("Entries() order/content unstable at index %d: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func TestEntriesSortsEBSVolumeRows(t *testing.T) {
	isolateCacheHome(t)
	p := New("us-east-1")
	var ebs []string
	for _, e := range p.Entries() {
		if len(e.SKU) > len("EBS volume ") && e.SKU[:len("EBS volume ")] == "EBS volume " {
			ebs = append(ebs, e.SKU)
		}
	}
	if len(ebs) != len(defaultRates["us-east-1"].EBSVolume) {
		t.Fatalf("expected one row per EBS volume type, got %d rows for %d types",
			len(ebs), len(defaultRates["us-east-1"].EBSVolume))
	}
	if !sort.StringsAreSorted(ebs) {
		t.Errorf("EBS volume rows must be sorted, got %v", ebs)
	}
}

func TestEntriesAllCarryAUnit(t *testing.T) {
	isolateCacheHome(t)
	p := New("us-east-1")
	entries := p.Entries()
	if len(entries) == 0 {
		t.Fatal("Entries() returned nothing")
	}
	for _, e := range entries {
		if e.SKU == "" {
			t.Errorf("entry with empty SKU: %+v", e)
		}
		if e.Unit == "" {
			t.Errorf("entry %q has no unit", e.SKU)
		}
	}
}

func TestDefaultRatesAreSaneAndNonNegative(t *testing.T) {
	rt := defaultRates["us-east-1"]
	if rt == nil {
		t.Fatal("us-east-1 must be present in defaultRates")
	}
	maps := map[string]map[string]USD{
		"EBSVolume":        rt.EBSVolume,
		"RDSInstanceMonth": rt.RDSInstanceMonth,
		"ElastiCacheNode":  rt.ElastiCacheNode,
		"OpenSearchNode":   rt.OpenSearchNode,
		"EC2InstanceMonth": rt.EC2InstanceMonth,
		"BedrockMUHour":    rt.BedrockMUHour,
		"S3StorageGB":      rt.S3StorageGB,
	}
	for name, m := range maps {
		if len(m) == 0 {
			t.Errorf("rate map %s is empty", name)
		}
		for k, v := range m {
			if k == "" {
				t.Errorf("rate map %s has an empty key", name)
			}
			if v < 0 {
				t.Errorf("rate map %s[%q] is negative: %v", name, k, float64(v))
			}
		}
	}
	scalars := map[string]USD{
		"EBSSnapshot":       rt.EBSSnapshot,
		"ElasticIP":         rt.ElasticIP,
		"NATGatewayHour":    rt.NATGatewayHour,
		"ALBHour":           rt.ALBHour,
		"ClassicELBMonth":   rt.ClassicELBMonth,
		"CloudWatchLogsGB":  rt.CloudWatchLogsGB,
		"EFSStandardGB":     rt.EFSStandardGB,
		"ECRGB":             rt.ECRGB,
		"Route53ZoneMonth":  rt.Route53ZoneMonth,
		"InterfaceEndpoint": rt.InterfaceEndpoint,
		"ClientVPNEndpoint": rt.ClientVPNEndpoint,
		"SiteToSiteVPN":     rt.SiteToSiteVPN,
		"LambdaPCGBSecond":  rt.LambdaPCGBSecond,
		"DynamoDBStorageGB": rt.DynamoDBStorageGB,
		"S3StandardGB":      rt.S3StandardGB,
	}
	for name, v := range scalars {
		if v <= 0 {
			t.Errorf("scalar rate %s must be positive, got %v", name, float64(v))
		}
	}
}

func TestEFSTiersAreOrderedCheapestLast(t *testing.T) {
	rt := defaultRates["us-east-1"]
	if rt.EFSStandardGB <= rt.EFSInfrequentGB {
		t.Errorf("EFS Standard (%v) should cost more than Infrequent Access (%v)",
			float64(rt.EFSStandardGB), float64(rt.EFSInfrequentGB))
	}
	if rt.EFSArchiveGB <= 0 {
		t.Errorf("EFS Archive rate must be set, got %v", float64(rt.EFSArchiveGB))
	}
}

func TestContainsFold(t *testing.T) {
	cases := []struct {
		haystack, needle string
		want             bool
	}{
		{"claude-3-opus", "opus", true},
		{"CLAUDE-3-OPUS", "opus", true},
		{"claude-3-opus", "OPUS", true},
		{"claude-3-opus", "claude", true},
		{"claude-3-opus", "sonnet", false},
		{"claude-3-5-sonnet", "claude-3-sonnet", false},
		{"anything", "", true},
		{"", "", true},
		{"short", "much-longer-needle", false},
		{"exact", "exact", true},
	}
	for _, tc := range cases {
		if got := containsFold(tc.haystack, tc.needle); got != tc.want {
			t.Errorf("containsFold(%q, %q) = %v, want %v", tc.haystack, tc.needle, got, tc.want)
		}
	}
}
