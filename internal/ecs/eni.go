package ecs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
)

// EC2API is the EC2 surface the ENI resolver uses.
type EC2API interface {
	DescribeNetworkInterfaces(ctx context.Context, in *awsec2.DescribeNetworkInterfacesInput, opts ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error)
}

// ENIResolver looks up a task ENI's public IP. Clients connect straight to
// the task's public address (no LB), but the RUNNING event
// only carries the ENI id and private IP.
type ENIResolver struct {
	api EC2API
}

// NewENIResolver returns an ENIResolver.
func NewENIResolver(api EC2API) *ENIResolver { return &ENIResolver{api: api} }

// PublicIP returns the ENI's associated public IP, or "" when it has none
// (private-subnet setups fall back to the private IP at the caller).
func (r *ENIResolver) PublicIP(ctx context.Context, eniID string) (string, error) {
	out, err := r.api.DescribeNetworkInterfaces(ctx, &awsec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []string{eniID},
	})
	if err != nil {
		return "", fmt.Errorf("describe eni %s: %w", eniID, err)
	}
	if len(out.NetworkInterfaces) == 0 {
		return "", fmt.Errorf("eni %s not found", eniID)
	}
	ni := out.NetworkInterfaces[0]
	if ni.Association == nil {
		return "", nil
	}
	return aws.ToString(ni.Association.PublicIp), nil
}
