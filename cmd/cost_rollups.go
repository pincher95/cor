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
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	efstypes "github.com/aws/aws-sdk-go-v2/service/efs/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
	elbv1types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/pincher95/cor/pkg/cost"
	"golang.org/x/sync/errgroup"
)

// Each rollupX builds a minimal OrphanPipeline (List + Process + MonthlyCost)
// and runs it via runOrphanRollup. The output is suppressed — only the count
// and aggregated cost matter.

func (a *AWSCommand) rollupVolumes(ctx context.Context) (int, cost.USD, error) {
	return runOrphanRollup(a, ctx, OrphanPipeline[ec2types.Volume, orphanVolume]{
		List: func(ctx context.Context, emit func(ec2types.Volume) error) error {
			p := ec2.NewDescribeVolumesPaginator(a.AWSClient.EC2, &ec2.DescribeVolumesInput{
				Filters: []ec2types.Filter{{Name: aws.String("status"), Values: []string{"available"}}},
			})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, v := range page.Volumes {
					if err := emit(v); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, vol ec2types.Volume) (*orphanVolume, error) {
			return &orphanVolume{
				id:         aws.ToString(vol.VolumeId),
				volumeType: string(vol.VolumeType),
				size:       aws.ToInt32(vol.Size),
			}, nil
		},
		MonthlyCost: func(r orphanVolume) cost.USD {
			return cost.USD(float64(r.size)) * a.Pricing.EBSVolumeGB(r.volumeType)
		},
	})
}

func (a *AWSCommand) rollupSnapshots(ctx context.Context) (int, cost.USD, error) {
	usedByImages := map[string]bool{}
	usedByVolumes := map[string]bool{}
	return runOrphanRollup(a, ctx, OrphanPipeline[ec2types.Snapshot, orphanSnapshot]{
		PreScan: func(ctx context.Context) error {
			g, gctx := errgroup.WithContext(ctx)
			g.Go(func() error {
				m, err := a.collectSnapshotsUsedByImages(gctx)
				if err != nil {
					return err
				}
				usedByImages = m
				return nil
			})
			g.Go(func() error {
				m, err := a.collectSnapshotsUsedByVolumes(gctx)
				if err != nil {
					return err
				}
				usedByVolumes = m
				return nil
			})
			return g.Wait()
		},
		List: func(ctx context.Context, emit func(ec2types.Snapshot) error) error {
			p := ec2.NewDescribeSnapshotsPaginator(a.AWSClient.EC2, &ec2.DescribeSnapshotsInput{
				OwnerIds: []string{"self"},
			})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, s := range page.Snapshots {
					if err := emit(s); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, s ec2types.Snapshot) (*orphanSnapshot, error) {
			id := aws.ToString(s.SnapshotId)
			if id == "" || usedByImages[id] || usedByVolumes[id] {
				return nil, nil
			}
			return &orphanSnapshot{id: id, size: aws.ToInt32(s.VolumeSize)}, nil
		},
		MonthlyCost: func(r orphanSnapshot) cost.USD {
			return cost.USD(float64(r.size)) * a.Pricing.EBSSnapshotGB()
		},
	})
}

func (a *AWSCommand) rollupElasticIPs(ctx context.Context) (int, cost.USD, error) {
	return runOrphanRollup(a, ctx, OrphanPipeline[ec2types.Address, orphanElasticIP]{
		List: func(ctx context.Context, emit func(ec2types.Address) error) error {
			out, err := a.AWSClient.EC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{})
			if err != nil {
				return err
			}
			for _, addr := range out.Addresses {
				if err := emit(addr); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, addr ec2types.Address) (*orphanElasticIP, error) {
			if addr.AssociationId != nil || addr.InstanceId != nil {
				return nil, nil
			}
			return &orphanElasticIP{allocationID: aws.ToString(addr.AllocationId)}, nil
		},
		MonthlyCost: func(_ orphanElasticIP) cost.USD { return a.Pricing.ElasticIPMonth() },
	})
}

func (a *AWSCommand) rollupNatGateways(ctx context.Context) (int, cost.USD, error) {
	return runOrphanRollup(a, ctx, OrphanPipeline[ec2types.NatGateway, orphanNatGateway]{
		List: func(ctx context.Context, emit func(ec2types.NatGateway) error) error {
			p := ec2.NewDescribeNatGatewaysPaginator(a.AWSClient.EC2, &ec2.DescribeNatGatewaysInput{
				Filter: []ec2types.Filter{{Name: aws.String("state"), Values: []string{"available"}}},
			})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, ng := range page.NatGateways {
					if err := emit(ng); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, ng ec2types.NatGateway) (*orphanNatGateway, error) {
			return &orphanNatGateway{id: aws.ToString(ng.NatGatewayId)}, nil
		},
		MonthlyCost: func(_ orphanNatGateway) cost.USD {
			return cost.USD(cost.HoursPerMonth) * a.Pricing.NATGatewayHour()
		},
	})
}

func (a *AWSCommand) rollupElbv2(ctx context.Context) (int, cost.USD, error) {
	return runOrphanRollup(a, ctx, OrphanPipeline[elbv2types.LoadBalancer, orphanLBv2]{
		List: func(ctx context.Context, emit func(elbv2types.LoadBalancer) error) error {
			p := elasticloadbalancingv2.NewDescribeLoadBalancersPaginator(a.AWSClient.ELB, &elasticloadbalancingv2.DescribeLoadBalancersInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, lb := range page.LoadBalancers {
					if err := emit(lb); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, lb elbv2types.LoadBalancer) (*orphanLBv2, error) {
			tgs, err := a.getTargetGroups(ctx, lb.LoadBalancerArn)
			if err != nil {
				return nil, err
			}
			eval, err := a.processLoadBalancer(ctx, tgs)
			if err != nil || eval == nil {
				return nil, err
			}
			if eval.hasExistingTargets || len(eval.orphanTargetGroups) == 0 {
				return nil, nil
			}
			return &orphanLBv2{lbArn: aws.ToString(lb.LoadBalancerArn), lbType: lb.Type}, nil
		},
		MonthlyCost: func(r orphanLBv2) cost.USD {
			switch r.lbType {
			case elbv2types.LoadBalancerTypeEnumApplication, elbv2types.LoadBalancerTypeEnumNetwork:
				return cost.USD(cost.HoursPerMonth) * a.Pricing.ALBHour()
			case elbv2types.LoadBalancerTypeEnumGateway:
				return cost.USD(cost.HoursPerMonth) * a.Pricing.GatewayLBHour()
			}
			return 0
		},
	})
}

func (a *AWSCommand) rollupElbv1(ctx context.Context) (int, cost.USD, error) {
	return runOrphanRollup(a, ctx, OrphanPipeline[elbv1types.LoadBalancerDescription, orphanLBv1]{
		List: func(ctx context.Context, emit func(elbv1types.LoadBalancerDescription) error) error {
			p := elasticloadbalancing.NewDescribeLoadBalancersPaginator(a.AWSClient.ELBv1, &elasticloadbalancing.DescribeLoadBalancersInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, lb := range page.LoadBalancerDescriptions {
					if err := emit(lb); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, lb elbv1types.LoadBalancerDescription) (*orphanLBv1, error) {
			eval, err := a.evaluateElbv1(ctx, &lb)
			if err != nil || eval == nil {
				return nil, err
			}
			if !eval.noTargets && (eval.hasExistingTargets || len(eval.orphanTargets) == 0) {
				return nil, nil
			}
			return &orphanLBv1{name: aws.ToString(lb.LoadBalancerName)}, nil
		},
		MonthlyCost: func(_ orphanLBv1) cost.USD { return a.Pricing.ClassicELBMonth() },
	})
}

func (a *AWSCommand) rollupLogs(ctx context.Context) (int, cost.USD, error) {
	return runOrphanRollup(a, ctx, OrphanPipeline[cwltypes.LogGroup, orphanLogGroup]{
		List: func(ctx context.Context, emit func(cwltypes.LogGroup) error) error {
			p := cloudwatchlogs.NewDescribeLogGroupsPaginator(a.AWSClient.CWL, &cloudwatchlogs.DescribeLogGroupsInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, lg := range page.LogGroups {
					if err := emit(lg); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, lg cwltypes.LogGroup) (*orphanLogGroup, error) {
			stored := int64(0)
			if lg.StoredBytes != nil {
				stored = *lg.StoredBytes
			}
			return &orphanLogGroup{name: aws.ToString(lg.LogGroupName), storedBytes: stored}, nil
		},
		MonthlyCost: func(r orphanLogGroup) cost.USD {
			gb := float64(r.storedBytes) / (1024 * 1024 * 1024)
			return cost.USD(gb) * a.Pricing.CloudWatchLogsGB()
		},
	})
}

func (a *AWSCommand) rollupEFS(ctx context.Context) (int, cost.USD, error) {
	return runOrphanRollup(a, ctx, OrphanPipeline[efstypes.FileSystemDescription, orphanEFS]{
		List: func(ctx context.Context, emit func(efstypes.FileSystemDescription) error) error {
			p := efs.NewDescribeFileSystemsPaginator(a.AWSClient.EFS, &efs.DescribeFileSystemsInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, fs := range page.FileSystems {
					if err := emit(fs); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, fs efstypes.FileSystemDescription) (*orphanEFS, error) {
			if fs.NumberOfMountTargets > 0 {
				return nil, nil
			}
			sz := int64(0)
			if fs.SizeInBytes != nil {
				sz = fs.SizeInBytes.Value
			}
			return &orphanEFS{id: aws.ToString(fs.FileSystemId), sizeBytes: sz}, nil
		},
		MonthlyCost: func(r orphanEFS) cost.USD {
			gb := float64(r.sizeBytes) / (1024 * 1024 * 1024)
			return cost.USD(gb) * a.Pricing.EFSStandardGB()
		},
	})
}

func (a *AWSCommand) rollupRoute53(ctx context.Context) (int, cost.USD, error) {
	return runOrphanRollup(a, ctx, OrphanPipeline[r53types.HostedZone, orphanRoute53Zone]{
		List: func(ctx context.Context, emit func(r53types.HostedZone) error) error {
			p := route53.NewListHostedZonesPaginator(a.AWSClient.R53, &route53.ListHostedZonesInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, z := range page.HostedZones {
					if err := emit(z); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(ctx context.Context, z r53types.HostedZone) (*orphanRoute53Zone, error) {
			orphan, err := a.isRoute53ZoneOrphan(ctx, z.Id)
			if err != nil || !orphan {
				return nil, err
			}
			return &orphanRoute53Zone{id: aws.ToString(z.Id), orphan: true}, nil
		},
		MonthlyCost: func(_ orphanRoute53Zone) cost.USD { return a.Pricing.Route53ZoneMonth() },
	})
}

func (a *AWSCommand) rollupVPCEndpoints(ctx context.Context) (int, cost.USD, error) {
	return runOrphanRollup(a, ctx, OrphanPipeline[ec2types.VpcEndpoint, orphanVPCEndpoint]{
		List: func(ctx context.Context, emit func(ec2types.VpcEndpoint) error) error {
			p := ec2.NewDescribeVpcEndpointsPaginator(a.AWSClient.EC2, &ec2.DescribeVpcEndpointsInput{
				Filters: []ec2types.Filter{{
					Name:   aws.String("vpc-endpoint-type"),
					Values: []string{string(ec2types.VpcEndpointTypeInterface)},
				}},
			})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, ep := range page.VpcEndpoints {
					if err := emit(ep); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Process: func(_ context.Context, ep ec2types.VpcEndpoint) (*orphanVPCEndpoint, error) {
			if len(ep.NetworkInterfaceIds) > 0 {
				return nil, nil
			}
			return &orphanVPCEndpoint{
				id:      aws.ToString(ep.VpcEndpointId),
				epType:  ep.VpcEndpointType,
				subnets: len(ep.SubnetIds),
			}, nil
		},
		MonthlyCost: func(r orphanVPCEndpoint) cost.USD {
			if r.epType != ec2types.VpcEndpointTypeInterface {
				return 0
			}
			n := r.subnets
			if n == 0 {
				n = 1
			}
			return cost.USD(n) * a.Pricing.InterfaceEndpointMonth()
		},
	})
}

func (a *AWSCommand) rollupVPNConnections(ctx context.Context) (int, cost.USD, error) {
	return runOrphanRollup(a, ctx, OrphanPipeline[ec2types.VpnConnection, orphanVPNConnection]{
		List: func(ctx context.Context, emit func(ec2types.VpnConnection) error) error {
			page, err := a.AWSClient.EC2.DescribeVpnConnections(ctx, &ec2.DescribeVpnConnectionsInput{})
			if err != nil {
				return err
			}
			for _, vpn := range page.VpnConnections {
				if err := emit(vpn); err != nil {
					return err
				}
			}
			return nil
		},
		Process: func(_ context.Context, vpn ec2types.VpnConnection) (*orphanVPNConnection, error) {
			if countVPNTunnelsUp(vpn) > 0 {
				return nil, nil
			}
			return &orphanVPNConnection{id: aws.ToString(vpn.VpnConnectionId)}, nil
		},
		MonthlyCost: func(_ orphanVPNConnection) cost.USD { return a.Pricing.SiteToSiteVPNMonth() },
	})
}

func (a *AWSCommand) rollupRDS(ctx context.Context) (int, cost.USD, error) {
	producers := []func(context.Context, func(rdsResource) error) error{
		func(ctx context.Context, emit func(rdsResource) error) error {
			p := rds.NewDescribeDBInstancesPaginator(a.AWSClient.RDS, &rds.DescribeDBInstancesInput{})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, inst := range page.DBInstances {
					if aws.ToString(inst.DBInstanceStatus) != "stopped" {
						continue
					}
					if err := emit(rdsResource{
						Type:   rdsInstance,
						ID:     aws.ToString(inst.DBInstanceIdentifier),
						Class:  aws.ToString(inst.DBInstanceClass),
						SizeGB: aws.ToInt32(inst.AllocatedStorage),
					}); err != nil {
						return err
					}
				}
			}
			return nil
		},
		func(ctx context.Context, emit func(rdsResource) error) error {
			p := rds.NewDescribeDBSnapshotsPaginator(a.AWSClient.RDS, &rds.DescribeDBSnapshotsInput{
				SnapshotType: aws.String("manual"),
			})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, snap := range page.DBSnapshots {
					if err := emit(rdsResource{
						Type:   rdsSnapshot,
						ID:     aws.ToString(snap.DBSnapshotIdentifier),
						SizeGB: aws.ToInt32(snap.AllocatedStorage),
					}); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
	return runOrphanRollup(a, ctx, OrphanPipeline[rdsResource, rdsResource]{
		Lists:   producers,
		Process: func(_ context.Context, r rdsResource) (*rdsResource, error) { return &r, nil },
		MonthlyCost: func(r rdsResource) cost.USD {
			switch r.Type {
			case rdsInstance:
				return a.Pricing.RDSInstanceClassMonth(r.Class) * 0.5
			case rdsSnapshot:
				return cost.USD(float64(r.SizeGB)) * a.Pricing.RDSManualSnapshotGB()
			}
			return 0
		},
	})
}
