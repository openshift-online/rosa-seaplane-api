package discovery

import (
	"context"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/customeraws"
)

type EC2API interface {
	DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
}

type VPC struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	CIDRBlock     string `json:"cidrBlock,omitempty"`
	RedHatManaged bool   `json:"redHatManaged"`
}

type VPCList struct {
	Kind  string `json:"kind"`
	Page  int    `json:"page"`
	Size  int    `json:"size"`
	Total int    `json:"total"`
	Items []VPC  `json:"items"`
}

// checkedEC2 rejects nil responses before the SDK paginator dereferences them.
type checkedEC2 struct{ EC2API }

func (c checkedEC2) DescribeVpcs(ctx context.Context, input *ec2.DescribeVpcsInput, opts ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	output, err := c.EC2API.DescribeVpcs(ctx, input, opts...)
	if err == nil && output == nil {
		return nil, &customeraws.Error{Kind: customeraws.InvalidResponse}
	}
	return output, err
}

// ListVPCs returns the complete inventory visible to this customer client.
// It never returns a partial list as a successful response.
func ListVPCs(ctx context.Context, client EC2API) (*VPCList, error) {
	result := &VPCList{Kind: "VPCList", Page: 1, Items: []VPC{}}
	pages := ec2.NewDescribeVpcsPaginator(checkedEC2{client}, &ec2.DescribeVpcsInput{MaxResults: aws.Int32(100)})
	seen := make(map[string]bool)
	for pages.HasMorePages() {
		output, err := pages.NextPage(ctx)
		if err != nil {
			return nil, customeraws.Classify(err, customeraws.DependencyUnavailable)
		}
		if token := aws.ToString(output.NextToken); token != "" {
			if seen[token] {
				return nil, &customeraws.Error{Kind: customeraws.InvalidResponse}
			}
			seen[token] = true
		}
		for _, item := range output.Vpcs {
			if aws.ToString(item.VpcId) == "" {
				return nil, &customeraws.Error{Kind: customeraws.InvalidResponse}
			}
			vpc := VPC{ID: aws.ToString(item.VpcId), CIDRBlock: aws.ToString(item.CidrBlock)}
			for _, tag := range item.Tags {
				switch aws.ToString(tag.Key) {
				case "Name":
					vpc.Name = aws.ToString(tag.Value)
				case "red-hat-managed":
					vpc.RedHatManaged, _ = strconv.ParseBool(aws.ToString(tag.Value))
				}
			}
			result.Items = append(result.Items, vpc)
		}
	}
	result.Size, result.Total = len(result.Items), len(result.Items)
	return result, nil
}
