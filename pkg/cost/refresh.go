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
	"fmt"
	"maps"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// RefreshRegion pulls list prices for the given region from the AWS
// Pricing API and returns a populated rateTable. The result starts from
// the hardcoded us-east-1 defaults and overlays any SKUs we successfully
// fetched — partial failures don't fail the whole refresh, so callers
// always get a usable table back.
//
// AWS hosts the Pricing API only in us-east-1 / ap-south-1, regardless of
// which region's prices you want; this function forces the client there.
func RefreshRegion(ctx context.Context, cfg aws.Config, region string) (*rateTable, error) {
	pricingCfg := cfg
	pricingCfg.Region = "us-east-1"
	client := pricing.NewFromConfig(pricingCfg)

	// Start from the bundled us-east-1 defaults so SKUs we don't fetch
	// here (S3 classes, Bedrock, Lambda PC, etc.) still have sane values.
	seed := usEast1Rates()
	rt := *seed
	rt.EBSVolume = cloneStringUSD(seed.EBSVolume)
	rt.EC2InstanceMonth = cloneStringUSD(seed.EC2InstanceMonth)
	rt.RDSInstanceMonth = cloneStringUSD(seed.RDSInstanceMonth)
	rt.ElastiCacheNode = cloneStringUSD(seed.ElastiCacheNode)
	rt.OpenSearchNode = cloneStringUSD(seed.OpenSearchNode)
	rt.S3StorageGB = cloneStringUSD(seed.S3StorageGB)
	rt.BedrockMUHour = cloneStringUSD(seed.BedrockMUHour)

	// Each fetcher is independent; collect errors but don't abort.
	if err := fetchEC2Instances(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("ec2 instances: %w", err)
	}
	if err := fetchEBSVolumes(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("ebs volumes: %w", err)
	}
	if err := fetchEBSSnapshot(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("ebs snapshot: %w", err)
	}
	if err := fetchElasticIP(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("eip: %w", err)
	}
	if err := fetchNATGateway(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("nat gateway: %w", err)
	}
	if err := fetchRDS(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("rds: %w", err)
	}
	if err := fetchELB(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("elb: %w", err)
	}
	if err := fetchS3(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("s3: %w", err)
	}
	if err := fetchEFS(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("efs: %w", err)
	}
	if err := fetchElastiCache(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("elasticache: %w", err)
	}
	if err := fetchOpenSearch(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("opensearch: %w", err)
	}
	if err := fetchCloudWatchLogs(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("cw logs: %w", err)
	}
	if err := fetchECR(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("ecr: %w", err)
	}
	if err := fetchRoute53(ctx, client, &rt); err != nil {
		return &rt, fmt.Errorf("route53: %w", err)
	}
	if err := fetchVPCEndpoint(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("vpc endpoint: %w", err)
	}
	if err := fetchClientVPN(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("client vpn: %w", err)
	}
	if err := fetchSiteToSiteVPN(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("s2s vpn: %w", err)
	}
	if err := fetchLambdaPC(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("lambda pc: %w", err)
	}
	if err := fetchDynamoDB(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("dynamodb: %w", err)
	}
	if err := fetchBedrock(ctx, client, region, &rt); err != nil {
		return &rt, fmt.Errorf("bedrock: %w", err)
	}
	return &rt, nil
}

func cloneStringUSD(in map[string]USD) map[string]USD {
	out := make(map[string]USD, len(in))
	maps.Copy(out, in)
	return out
}

// fetchEC2Instances pulls Linux/Shared/no-pre-install on-demand instance
// rates and stores them as $/month (730h) into rt.EC2InstanceMonth.
func fetchEC2Instances(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("productFamily", "Compute Instance"),
		eqFilter("regionCode", region),
		eqFilter("operatingSystem", "Linux"),
		eqFilter("tenancy", "Shared"),
		eqFilter("preInstalledSw", "NA"),
		eqFilter("capacitystatus", "Used"),
	}
	return forEachPrice(ctx, c, "AmazonEC2", filters, func(p priceItem) {
		typ, _ := p.attrString("instanceType")
		if typ == "" {
			return
		}
		hourly, ok := p.onDemandUSDPerHour()
		if !ok || hourly == 0 {
			return
		}
		rt.EC2InstanceMonth[typ] = hourly * HoursPerMonth
	})
}

// fetchEBSVolumes pulls $/GB-month for each provisioned volume type.
func fetchEBSVolumes(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("productFamily", "Storage"),
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonEC2", filters, func(p priceItem) {
		volType, _ := p.attrString("volumeApiName")
		if volType == "" {
			return
		}
		rate, ok := p.onDemandUSDPerUnit()
		if !ok || rate == 0 {
			return
		}
		rt.EBSVolume[volType] = rate
	})
}

// fetchEBSSnapshot pulls the $/GB-month rate for incremental EBS snapshots.
func fetchEBSSnapshot(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("productFamily", "Storage Snapshot"),
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonEC2", filters, func(p priceItem) {
		rate, ok := p.onDemandUSDPerUnit()
		if !ok || rate == 0 {
			return
		}
		// Multiple snapshot-related SKUs ship in this family (Archive,
		// Restore, GB-month). Prefer the standard GB-month line by usage type.
		ut, _ := p.attrString("usagetype")
		if rt.EBSSnapshot == 0 || (ut != "" && contains(ut, "EBS:Snapshots")) {
			rt.EBSSnapshot = rate
		}
	})
}

// fetchElasticIP pulls the per-hour rate for an unassociated EIP and
// stores monthly ($/hr × 730h).
func fetchElasticIP(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("productFamily", "IP Address"),
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonEC2", filters, func(p priceItem) {
		ut, _ := p.attrString("usagetype")
		// AWS unbundled idle-EIP into "ElasticIP:IdleAddress" usage types.
		if ut == "" || !contains(ut, "ElasticIP") {
			return
		}
		hourly, ok := p.onDemandUSDPerHour()
		if !ok || hourly == 0 {
			return
		}
		rt.ElasticIP = hourly * HoursPerMonth
	})
}

// fetchNATGateway pulls both the per-hour idle rate and the per-GB
// data-processing rate. The two SKUs sit in the same product family.
func fetchNATGateway(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("productFamily", "NAT Gateway"),
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonEC2", filters, func(p priceItem) {
		rate, ok := p.onDemandUSDPerUnit()
		if !ok || rate == 0 {
			return
		}
		ut, _ := p.attrString("usagetype")
		switch {
		case contains(ut, "Hours"):
			rt.NATGatewayHour = rate
		case contains(ut, "Bytes"):
			rt.NATGatewayDataGB = rate
		}
	})
}

// ----- Pricing API plumbing -----

func eqFilter(field, value string) pricingtypes.Filter {
	return pricingtypes.Filter{
		Type:  pricingtypes.FilterTypeTermMatch,
		Field: aws.String(field),
		Value: aws.String(value),
	}
}

// priceItem is one decoded entry from the PriceList JSON array.
type priceItem struct {
	raw map[string]any
}

func (p priceItem) attrString(key string) (string, bool) {
	product, _ := p.raw["product"].(map[string]any)
	attrs, _ := product["attributes"].(map[string]any)
	v, ok := attrs[key].(string)
	return v, ok
}

// onDemandUSDPerHour returns the on-demand USD price assuming the price
// dimension's unit is hourly. Caller may multiply by 730 for monthly.
func (p priceItem) onDemandUSDPerHour() (USD, bool) {
	return p.onDemandUSDPerUnit()
}

// onDemandUSDPerUnit returns the first OnDemand USD price found regardless
// of unit. Callers that care about the unit can dig into the raw map.
func (p priceItem) onDemandUSDPerUnit() (USD, bool) {
	terms, _ := p.raw["terms"].(map[string]any)
	onDemand, _ := terms["OnDemand"].(map[string]any)
	for _, term := range onDemand {
		t, _ := term.(map[string]any)
		dims, _ := t["priceDimensions"].(map[string]any)
		for _, dim := range dims {
			d, _ := dim.(map[string]any)
			pp, _ := d["pricePerUnit"].(map[string]any)
			raw, _ := pp["USD"].(string)
			if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
				return USD(v), true
			}
		}
	}
	return 0, false
}

// forEachPrice paginates GetProducts for the given service+filters and
// invokes fn once per decoded item. Decode errors on individual items are
// silently skipped — better to keep all the good prices than to bail on
// the one weird SKU.
func forEachPrice(
	ctx context.Context,
	c *pricing.Client,
	serviceCode string,
	filters []pricingtypes.Filter,
	fn func(priceItem),
) error {
	p := pricing.NewGetProductsPaginator(c, &pricing.GetProductsInput{
		ServiceCode: aws.String(serviceCode),
		Filters:     filters,
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, raw := range page.PriceList {
			var item map[string]any
			if err := json.Unmarshal([]byte(raw), &item); err != nil {
				continue
			}
			fn(priceItem{raw: item})
		}
	}
	return nil
}

// fetchRDS pulls per-instance hourly rates for PostgreSQL Single-AZ
// (cheapest engine; close enough for all engines for orphan triage)
// and the manual-snapshot $/GB-month rate.
func fetchRDS(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	instFilters := []pricingtypes.Filter{
		eqFilter("productFamily", "Database Instance"),
		eqFilter("regionCode", region),
		eqFilter("databaseEngine", "PostgreSQL"),
		eqFilter("deploymentOption", "Single-AZ"),
	}
	if err := forEachPrice(ctx, c, "AmazonRDS", instFilters, func(p priceItem) {
		typ, _ := p.attrString("instanceType")
		if typ == "" {
			return
		}
		hourly, ok := p.onDemandUSDPerHour()
		if !ok || hourly == 0 {
			return
		}
		rt.RDSInstanceMonth[typ] = hourly * HoursPerMonth
	}); err != nil {
		return err
	}

	// Multiple "Storage Snapshot" SKUs exist under AmazonRDS — manual /
	// automated backup ("ChargedBackupUsage") is what we want; Aurora's
	// own backup SKU ("Aurora:BackupUsage") prices at $0.021/GB-mo which
	// is different from $0.095 for RDS manual snapshots.
	snapFilters := []pricingtypes.Filter{
		eqFilter("productFamily", "Storage Snapshot"),
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonRDS", snapFilters, func(p priceItem) {
		ut, _ := p.attrString("usagetype")
		// Skip Aurora and any RDSCustom variant; we want the canonical
		// RDS manual snapshot rate.
		if !contains(ut, "ChargedBackupUsage") || contains(ut, "RDSCustom") {
			return
		}
		rate, ok := p.onDemandUSDPerUnit()
		if !ok || rate == 0 {
			return
		}
		rt.RDSManualSnapGB = rate
	})
}

// fetchELB covers ALB/NLB hourly + LCU, GWLB hourly, and Classic ELB
// (which AWS prices per-hour; we store monthly).
func fetchELB(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	families := []string{
		"Load Balancer-Application",
		"Load Balancer-Network",
		"Load Balancer-Gateway",
		"Load Balancer",
	}
	for _, fam := range families {
		filters := []pricingtypes.Filter{
			eqFilter("productFamily", fam),
			eqFilter("regionCode", region),
		}
		if err := forEachPrice(ctx, c, "AWSELB", filters, func(p priceItem) {
			rate, ok := p.onDemandUSDPerUnit()
			if !ok || rate == 0 {
				return
			}
			ut, _ := p.attrString("usagetype")
			switch {
			case contains(ut, "LCU"):
				// ALB and NLB both have LCU rates; ALB is what we care about.
				if fam == "Load Balancer-Application" {
					rt.ALBLCUHour = rate
				}
			case fam == "Load Balancer-Application" && contains(ut, "Hours"):
				rt.ALBHour = rate
			case fam == "Load Balancer-Gateway" && contains(ut, "Hours"):
				rt.GatewayLBHour = rate
			case fam == "Load Balancer" && contains(ut, "Hours"):
				// Classic ELB is priced hourly; surface as monthly.
				rt.ClassicELBMonth = rate * HoursPerMonth
			}
		}); err != nil {
			return err
		}
	}
	return nil
}

// s3VolumeTypeToCWClass maps the AWS Pricing API's `volumeType` attribute
// to the CloudWatch StorageType dimension string that callers query.
var s3VolumeTypeToCWClass = map[string]string{
	"Standard":                                   "StandardStorage",
	"Standard - Infrequent Access":               "StandardIAStorage",
	"One Zone - Infrequent Access":               "OneZoneIAStorage",
	"Amazon Glacier":                             "GlacierStorage",
	"Amazon Glacier Instant Retrieval":           "GlacierInstantRetrievalStorage",
	"Amazon Glacier Deep Archive":                "DeepArchiveStorage",
	"Intelligent-Tiering Frequent Access":        "IntelligentTieringFAStorage",
	"Intelligent-Tiering Infrequent Access":      "IntelligentTieringIAStorage",
	"Intelligent-Tiering Archive Access":         "IntelligentTieringAAStorage",
	"Intelligent-Tiering Archive Instant Access": "IntelligentTieringAIAStorage",
	"Intelligent-Tiering Deep Archive Access":    "IntelligentTieringDAAStorage",
}

// fetchS3 populates the per-storage-class rates. S3 has tiered pricing
// (First 50 TB, Next 450 TB, ...); the first tier rate is what cor uses
// since `cor s3buckets` divides BucketSizeBytes by the first-tier rate.
func fetchS3(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("productFamily", "Storage"),
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonS3", filters, func(p priceItem) {
		vt, _ := p.attrString("volumeType")
		cwClass, ok := s3VolumeTypeToCWClass[vt]
		if !ok {
			return
		}
		rate, ok := s3FirstTierUSD(p)
		if !ok || rate == 0 {
			return
		}
		rt.S3StorageGB[cwClass] = rate
		// Legacy single-rate field — keep it pinned to Standard for any
		// caller that still hits S3StandardGB() directly.
		if vt == "Standard" {
			rt.S3StandardGB = rate
		}
	})
}

// s3FirstTierUSD picks the priceDimension with beginRange="0" — the first
// tier. Plain onDemandUSDPerUnit returns whichever dimension iterates first.
func s3FirstTierUSD(p priceItem) (USD, bool) {
	terms, _ := p.raw["terms"].(map[string]any)
	onDemand, _ := terms["OnDemand"].(map[string]any)
	for _, term := range onDemand {
		t, _ := term.(map[string]any)
		dims, _ := t["priceDimensions"].(map[string]any)
		for _, dim := range dims {
			d, _ := dim.(map[string]any)
			if begin, _ := d["beginRange"].(string); begin != "0" {
				continue
			}
			pp, _ := d["pricePerUnit"].(map[string]any)
			raw, _ := pp["USD"].(string)
			if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
				return USD(v), true
			}
		}
	}
	return 0, false
}

// fetchEFS populates Standard / Infrequent Access / Archive $/GB-month.
func fetchEFS(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("productFamily", "Storage"),
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonEFS", filters, func(p priceItem) {
		sc, _ := p.attrString("storageClass")
		rate, ok := p.onDemandUSDPerUnit()
		if !ok || rate == 0 {
			return
		}
		switch {
		case contains(sc, "Archive"):
			rt.EFSArchiveGB = rate
		case contains(sc, "Infrequent"):
			rt.EFSInfrequentGB = rate
		case sc == "General Purpose" || contains(sc, "Standard"):
			rt.EFSStandardGB = rate
		}
	})
}

// fetchElastiCache pulls Redis on-demand node rates as $/month.
func fetchElastiCache(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("productFamily", "Cache Instance"),
		eqFilter("regionCode", region),
		eqFilter("cacheEngine", "Redis"),
	}
	return forEachPrice(ctx, c, "AmazonElastiCache", filters, func(p priceItem) {
		typ, _ := p.attrString("instanceType")
		if typ == "" {
			return
		}
		hourly, ok := p.onDemandUSDPerHour()
		if !ok || hourly == 0 {
			return
		}
		rt.ElastiCacheNode[typ] = hourly * HoursPerMonth
	})
}

// fetchOpenSearch pulls node-type hourly rates and managed-service storage
// rate. AWS still labels the service code "AmazonES" in the Pricing API
// (legacy Elasticsearch name).
func fetchOpenSearch(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	nodeFilters := []pricingtypes.Filter{
		eqFilter("productFamily", "Amazon OpenSearch Service Instance"),
		eqFilter("regionCode", region),
	}
	if err := forEachPrice(ctx, c, "AmazonES", nodeFilters, func(p priceItem) {
		typ, _ := p.attrString("instanceType")
		if typ == "" {
			return
		}
		hourly, ok := p.onDemandUSDPerHour()
		if !ok || hourly == 0 {
			return
		}
		rt.OpenSearchNode[typ] = hourly * HoursPerMonth
	}); err != nil {
		return err
	}

	storageFilters := []pricingtypes.Filter{
		eqFilter("productFamily", "Amazon OpenSearch Service Volume"),
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonES", storageFilters, func(p priceItem) {
		// Multiple volume types exist (gp2/gp3/io1); use gp3 as the common case.
		st, _ := p.attrString("storageMedia")
		rate, ok := p.onDemandUSDPerUnit()
		if !ok || rate == 0 {
			return
		}
		if rt.OpenSearchStorageGB == 0 || contains(st, "SSD") {
			rt.OpenSearchStorageGB = rate
		}
	})
}

// fetchCloudWatchLogs populates $/GB-month for stored log data.
func fetchCloudWatchLogs(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("productFamily", "Storage Snapshot"),
		eqFilter("regionCode", region),
	}
	// CloudWatch Logs publishes log storage under productFamily=Storage Snapshot
	// in the AmazonCloudWatch service code.
	return forEachPrice(ctx, c, "AmazonCloudWatch", filters, func(p priceItem) {
		ut, _ := p.attrString("usagetype")
		if !contains(ut, "TimedStorage") {
			return
		}
		rate, ok := p.onDemandUSDPerUnit()
		if ok && rate > 0 {
			rt.CloudWatchLogsGB = rate
		}
	})
}

// fetchECR populates $/GB-month for ECR image storage.
func fetchECR(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonECR", filters, func(p priceItem) {
		ut, _ := p.attrString("usagetype")
		if !contains(ut, "Storage") {
			return
		}
		rate, ok := p.onDemandUSDPerUnit()
		if ok && rate > 0 {
			rt.ECRGB = rate
		}
	})
}

// fetchRoute53 populates $/month per hosted zone. R53 is global — no
// regionCode filter applies.
func fetchRoute53(ctx context.Context, c *pricing.Client, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("productFamily", "DNS Zone"),
	}
	return forEachPrice(ctx, c, "AmazonRoute53", filters, func(p priceItem) {
		ut, _ := p.attrString("usagetype")
		// First-zone rate ($0.50). Subsequent zones are cheaper but rare.
		if !contains(ut, "HostedZone") {
			return
		}
		rate, ok := s3FirstTierUSD(p) // reuse "first range" helper
		if !ok {
			rate, ok = p.onDemandUSDPerUnit()
			if !ok {
				return
			}
		}
		if rate > 0 {
			rt.Route53ZoneMonth = rate
		}
	})
}

// fetchVPCEndpoint populates $/month per AZ for an idle interface endpoint.
func fetchVPCEndpoint(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("productFamily", "VpcEndpoint"),
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonVPC", filters, func(p priceItem) {
		ut, _ := p.attrString("usagetype")
		// Endpoint-hour SKU, not bytes.
		if !contains(ut, "VpcEndpoint-Hours") && !contains(ut, "InterfaceEndpoint") {
			return
		}
		hourly, ok := p.onDemandUSDPerHour()
		if ok && hourly > 0 {
			rt.InterfaceEndpoint = hourly * HoursPerMonth
		}
	})
}

// fetchClientVPN populates $/month per Client VPN endpoint association.
func fetchClientVPN(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonVPC", filters, func(p priceItem) {
		ut, _ := p.attrString("usagetype")
		// Two relevant SKUs: AssociationHours (per associated subnet) and
		// ConnectionHours (per active user). Idle endpoint cost is dominated
		// by the association hours.
		if !contains(ut, "ClientVPN-AssociationHours") {
			return
		}
		hourly, ok := p.onDemandUSDPerHour()
		if ok && hourly > 0 {
			rt.ClientVPNEndpoint = hourly * HoursPerMonth
		}
	})
}

// fetchSiteToSiteVPN populates $/month per Site-to-Site VPN connection.
func fetchSiteToSiteVPN(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonVPC", filters, func(p priceItem) {
		ut, _ := p.attrString("usagetype")
		if !contains(ut, "VPN-Usage-Hours") && !contains(ut, "VpnConnection") {
			return
		}
		hourly, ok := p.onDemandUSDPerHour()
		if ok && hourly > 0 {
			rt.SiteToSiteVPN = hourly * HoursPerMonth
		}
	})
}

// fetchLambdaPC populates the $/GB-second rate for x86 provisioned
// concurrency. ARM (Graviton) PC is ~20% cheaper and indicated by a
// "-ARM" suffix; we ignore it to match the more common workload default.
func fetchLambdaPC(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AWSLambda", filters, func(p priceItem) {
		ut, _ := p.attrString("usagetype")
		if !contains(ut, "Provisioned-Concurrency") || contains(ut, "-ARM") {
			return
		}
		rate, ok := p.onDemandUSDPerUnit()
		if ok && rate > 0 {
			rt.LambdaPCGBSecond = rate
		}
	})
}

// fetchDynamoDB populates provisioned WCU/RCU hourly + storage $/GB-month.
// Standard-class only — IA-class (e.g. "IA-ReadCapacityUnit-Hrs") is
// excluded so the rates match the default table class.
func fetchDynamoDB(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonDynamoDB", filters, func(p priceItem) {
		ut, _ := p.attrString("usagetype")
		if contains(ut, "IA-") {
			return
		}
		rate, ok := p.onDemandUSDPerUnit()
		if !ok || rate == 0 {
			return
		}
		switch {
		case contains(ut, "WriteCapacityUnit-Hrs"):
			rt.DynamoDBWCUHour = rate
		case contains(ut, "ReadCapacityUnit-Hrs"):
			rt.DynamoDBRCUHour = rate
		case contains(ut, "TimedStorage-ByteHrs"):
			rt.DynamoDBStorageGB = rate
		}
	})
}

// fetchBedrock populates per-model-unit hourly rates for provisioned
// throughput. The Pricing API keys these by model+commitment-term; we
// store the no-commit ("OnDemand"-style PT) rate keyed by a model-id
// substring matching the existing BedrockMUHour map shape.
func fetchBedrock(ctx context.Context, c *pricing.Client, region string, rt *rateTable) error {
	filters := []pricingtypes.Filter{
		eqFilter("regionCode", region),
	}
	return forEachPrice(ctx, c, "AmazonBedrock", filters, func(p priceItem) {
		ut, _ := p.attrString("usagetype")
		// Provisioned-throughput SKUs include "-NoCommit-" in the usage
		// type; the regular on-demand token SKUs include "input-tokens" /
		// "output-tokens" which we skip here.
		if !contains(ut, "NoCommit") {
			return
		}
		model, _ := p.attrString("model")
		if model == "" {
			model, _ = p.attrString("modelId")
		}
		if model == "" {
			return
		}
		hourly, ok := p.onDemandUSDPerHour()
		if !ok || hourly == 0 {
			return
		}
		// Key the table by the substring that the cor pricing accessor
		// matches against (claude-3-haiku, claude-3-opus, …). Pick the
		// closest known key from the seed map; if no match, store the
		// raw model id as a new key.
		key := model
		for seeded := range rt.BedrockMUHour {
			if contains(model, seeded) {
				key = seeded
				break
			}
		}
		rt.BedrockMUHour[key] = hourly
	})
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
