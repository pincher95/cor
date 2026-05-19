/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cost

// rateTable holds per-region monthly/hourly USD rates by AWS SKU shape.
// Unknown fields default to zero, which renders as "—" — preferable to
// guessing a wrong number.
type rateTable struct {
	EBSVolume   map[string]USD // $/GB-month by volume type (gp3, io1, ...)
	EBSSnapshot USD            // $/GB-month
	ElasticIP   USD            // $/month per unassociated EIP

	NATGatewayHour      USD // $/hour idle
	NATGatewayDataGB    USD // $/GB processed
	ALBHour             USD // application/network load balancer $/hour
	ALBLCUHour          USD // $/LCU-hour — applies on top of base hour
	GatewayLBHour       USD
	ClassicELBMonth     USD // bundled monthly figure ($18.25-ish)
	RDSManualSnapGB     USD // $/GB-month
	RDSInstanceMonth    map[string]USD
	CloudWatchLogsGB    USD // $/GB-month stored
	EFSStandardGB       USD
	EFSInfrequentGB     USD
	EFSArchiveGB        USD
	ECRGB               USD
	OpenSearchStorageGB USD
	Route53ZoneMonth    USD
	InterfaceEndpoint   USD // $/AZ-month (idle); data not modeled
	ClientVPNEndpoint   USD // $/month per endpoint
	SiteToSiteVPN       USD // $/month per connection

	LambdaPCGBSecond  USD // provisioned concurrency $/GB-second
	ElastiCacheNode   map[string]USD
	OpenSearchNode    map[string]USD
	DynamoDBWCUHour   USD
	DynamoDBRCUHour   USD
	DynamoDBStorageGB USD
	S3StandardGB      USD
}

// defaultRates ships in code so cor works offline. Phase G adds a refresh
// path that overwrites these from the AWS Pricing API. Numbers are
// approximate (us-east-1 list prices, no RI/SP discounts).
var defaultRates = map[string]*rateTable{
	"us-east-1": usEast1Rates(),
}

func usEast1Rates() *rateTable {
	return &rateTable{
		EBSVolume: map[string]USD{
			"gp3":      0.08,
			"gp2":      0.10,
			"io1":      0.125,
			"io2":      0.125,
			"st1":      0.045,
			"sc1":      0.015,
			"standard": 0.05,
		},
		EBSSnapshot:         0.05,
		ElasticIP:           3.65,
		NATGatewayHour:      0.045,
		NATGatewayDataGB:    0.045,
		ALBHour:             0.0225,
		ALBLCUHour:          0.008,
		GatewayLBHour:       0.0125,
		ClassicELBMonth:     18.25,
		RDSManualSnapGB:     0.095,
		CloudWatchLogsGB:    0.03,
		EFSStandardGB:       0.30,
		EFSInfrequentGB:     0.0125,
		EFSArchiveGB:        0.045,
		ECRGB:               0.10,
		OpenSearchStorageGB: 0.122,
		Route53ZoneMonth:    0.50,
		InterfaceEndpoint:   7.30,
		ClientVPNEndpoint:   73.00,
		SiteToSiteVPN:       36.50,
		LambdaPCGBSecond:    0.0000041667,
		DynamoDBWCUHour:     0.00065,
		DynamoDBRCUHour:     0.00013,
		DynamoDBStorageGB:   0.25,
		S3StandardGB:        0.023,
		RDSInstanceMonth: map[string]USD{
			"db.t3.micro":  12.41,
			"db.t3.small":  24.82,
			"db.t3.medium": 49.64,
			"db.m5.large":  124.10,
			"db.m5.xlarge": 248.20,
			"db.r5.large":  167.90,
			"db.r5.xlarge": 335.80,
		},
		ElastiCacheNode: map[string]USD{
			"cache.t3.micro":   12.41,
			"cache.t3.small":   24.82,
			"cache.t3.medium":  49.64,
			"cache.m5.large":   105.85,
			"cache.r6g.large":  130.31,
			"cache.r6g.xlarge": 217.18,
		},
		OpenSearchNode: map[string]USD{
			"t3.small.search":  26.30,
			"t3.medium.search": 52.60,
			"m5.large.search":  102.20,
			"r5.large.search":  131.40,
			"r6g.large.search": 101.18,
		},
	}
}
