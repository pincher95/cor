/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// enabledRegions returns the regions enabled (or enabled-by-default) for the
// caller's account. Always uses the EC2 client from awsCmd, which is
// assumed to be present for the current region.
func enabledRegions(ctx context.Context, ec2Client *ec2.Client) ([]string, error) {
	out, err := ec2Client.DescribeRegions(ctx, &ec2.DescribeRegionsInput{
		AllRegions: aws.Bool(false),
	})
	if err != nil {
		return nil, err
	}
	regions := make([]string, 0, len(out.Regions))
	for _, r := range out.Regions {
		regions = append(regions, aws.ToString(r.RegionName))
	}
	return regions, nil
}
