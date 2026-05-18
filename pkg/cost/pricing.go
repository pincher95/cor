/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cost

import "sort"

// Pricing exposes per-region per-SKU rates. New(region) returns a Pricing
// initialized from the embedded defaults; unknown regions fall back to
// us-east-1.
type Pricing struct {
	region string
	rates  *rateTable
}

// New returns a Pricing for the given region.
func New(region string) *Pricing {
	r := defaultRates[region]
	if r == nil {
		r = defaultRates["us-east-1"]
	}
	return &Pricing{region: region, rates: r}
}

// Region returns the configured region.
func (p *Pricing) Region() string { return p.region }

// EBSVolumeGB returns $/GB-month for the given volume type.
func (p *Pricing) EBSVolumeGB(volumeType string) USD {
	if v, ok := p.rates.EBSVolume[volumeType]; ok {
		return v
	}
	return p.rates.EBSVolume["gp3"]
}

// EBSSnapshotGB returns $/GB-month for snapshots.
func (p *Pricing) EBSSnapshotGB() USD { return p.rates.EBSSnapshot }

// ElasticIPMonth returns $/month per unassociated EIP.
func (p *Pricing) ElasticIPMonth() USD { return p.rates.ElasticIP }

// NATGatewayHour returns $/hour idle.
func (p *Pricing) NATGatewayHour() USD { return p.rates.NATGatewayHour }

// NATGatewayDataGB returns $/GB processed.
func (p *Pricing) NATGatewayDataGB() USD { return p.rates.NATGatewayDataGB }

// ALBHour returns $/hour for application/network load balancers.
func (p *Pricing) ALBHour() USD { return p.rates.ALBHour }

// GatewayLBHour returns $/hour for gateway load balancers.
func (p *Pricing) GatewayLBHour() USD { return p.rates.GatewayLBHour }

// ClassicELBMonth returns $/month for a Classic ELB.
func (p *Pricing) ClassicELBMonth() USD { return p.rates.ClassicELBMonth }

// RDSManualSnapshotGB returns $/GB-month for manual RDS snapshots.
func (p *Pricing) RDSManualSnapshotGB() USD { return p.rates.RDSManualSnapGB }

// RDSInstanceClassMonth returns $/month for the given db.* instance class.
// Returns 0 (renders as "—") for unknown classes.
func (p *Pricing) RDSInstanceClassMonth(class string) USD {
	return p.rates.RDSInstanceMonth[class]
}

// CloudWatchLogsGB returns $/GB-month for stored log data.
func (p *Pricing) CloudWatchLogsGB() USD { return p.rates.CloudWatchLogsGB }

// EFSStandardGB returns $/GB-month for EFS Standard storage.
func (p *Pricing) EFSStandardGB() USD { return p.rates.EFSStandardGB }

// EFSInfrequentGB returns $/GB-month for EFS Infrequent Access storage.
func (p *Pricing) EFSInfrequentGB() USD { return p.rates.EFSInfrequentGB }

// ECRGB returns $/GB-month for container image storage.
func (p *Pricing) ECRGB() USD { return p.rates.ECRGB }

// Route53ZoneMonth returns $/month per hosted zone.
func (p *Pricing) Route53ZoneMonth() USD { return p.rates.Route53ZoneMonth }

// InterfaceEndpointMonth returns $/month per AZ for an interface VPC endpoint.
func (p *Pricing) InterfaceEndpointMonth() USD { return p.rates.InterfaceEndpoint }

// ClientVPNEndpointMonth returns $/month per Client VPN endpoint (idle).
func (p *Pricing) ClientVPNEndpointMonth() USD { return p.rates.ClientVPNEndpoint }

// SiteToSiteVPNMonth returns $/month per Site-to-Site VPN connection.
func (p *Pricing) SiteToSiteVPNMonth() USD { return p.rates.SiteToSiteVPN }

// LambdaProvisionedConcurrencyGBSecond returns $/GB-second for Lambda
// provisioned concurrency. Multiply by memory (GB) × HoursPerMonth × 3600.
func (p *Pricing) LambdaProvisionedConcurrencyGBSecond() USD {
	return p.rates.LambdaPCGBSecond
}

// ElastiCacheNodeMonth returns $/month for the given node type.
func (p *Pricing) ElastiCacheNodeMonth(nodeType string) USD {
	return p.rates.ElastiCacheNode[nodeType]
}

// OpenSearchNodeMonth returns $/month for the given OpenSearch instance type.
func (p *Pricing) OpenSearchNodeMonth(instanceType string) USD {
	return p.rates.OpenSearchNode[instanceType]
}

// DynamoDBWCUMonth returns $/month for one provisioned WCU.
func (p *Pricing) DynamoDBWCUMonth() USD { return p.rates.DynamoDBWCUHour * HoursPerMonth }

// DynamoDBRCUMonth returns $/month for one provisioned RCU.
func (p *Pricing) DynamoDBRCUMonth() USD { return p.rates.DynamoDBRCUHour * HoursPerMonth }

// DynamoDBStorageGB returns $/GB-month for DynamoDB storage.
func (p *Pricing) DynamoDBStorageGB() USD { return p.rates.DynamoDBStorageGB }

// S3StandardGB returns $/GB-month for S3 Standard storage.
func (p *Pricing) S3StandardGB() USD { return p.rates.S3StandardGB }

// PriceEntry is one row of the embedded rate table for display.
type PriceEntry struct {
	SKU  string
	Rate USD
	Unit string
}

// Entries returns the rates table flattened for display. Used by
// `cor pricing show`. Order is stable across calls (EBS volume rows are
// sorted by type so map iteration randomness doesn't leak into output).
func (p *Pricing) Entries() []PriceEntry {
	r := p.rates
	rows := []PriceEntry{
		{"EBS snapshot", r.EBSSnapshot, "$/GB-month"},
		{"Elastic IP (unassociated)", r.ElasticIP, "$/month"},
		{"NAT Gateway (idle)", r.NATGatewayHour, "$/hour"},
		{"NAT Gateway (data)", r.NATGatewayDataGB, "$/GB"},
		{"ALB / NLB", r.ALBHour, "$/hour"},
		{"Gateway LB", r.GatewayLBHour, "$/hour"},
		{"Classic ELB", r.ClassicELBMonth, "$/month"},
		{"RDS manual snapshot", r.RDSManualSnapGB, "$/GB-month"},
		{"CloudWatch Logs storage", r.CloudWatchLogsGB, "$/GB-month"},
		{"EFS Standard", r.EFSStandardGB, "$/GB-month"},
		{"EFS Infrequent Access", r.EFSInfrequentGB, "$/GB-month"},
		{"ECR storage", r.ECRGB, "$/GB-month"},
		{"Route53 hosted zone", r.Route53ZoneMonth, "$/month"},
		{"VPC interface endpoint", r.InterfaceEndpoint, "$/month per AZ"},
		{"Client VPN endpoint", r.ClientVPNEndpoint, "$/month"},
		{"Site-to-Site VPN", r.SiteToSiteVPN, "$/month"},
		{"Lambda provisioned concurrency", r.LambdaPCGBSecond, "$/GB-second"},
		{"DynamoDB WCU (provisioned)", r.DynamoDBWCUHour, "$/hour"},
		{"DynamoDB RCU (provisioned)", r.DynamoDBRCUHour, "$/hour"},
		{"DynamoDB storage", r.DynamoDBStorageGB, "$/GB-month"},
		{"S3 Standard storage", r.S3StandardGB, "$/GB-month"},
	}
	types := make([]string, 0, len(r.EBSVolume))
	for vt := range r.EBSVolume {
		types = append(types, vt)
	}
	sort.Strings(types)
	for _, vt := range types {
		rows = append(rows, PriceEntry{"EBS volume " + vt, r.EBSVolume[vt], "$/GB-month"})
	}
	return rows
}
