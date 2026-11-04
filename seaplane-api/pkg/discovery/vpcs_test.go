package discovery

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/customeraws"
)

type vpcPages struct {
	pages []*ec2.DescribeVpcsOutput
	calls int
}

func (f *vpcPages) DescribeVpcs(_ context.Context, input *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	if f.calls > 0 && aws.ToString(input.NextToken) != "next" {
		return nil, errors.New("missing pagination token")
	}
	page := f.pages[f.calls]
	f.calls++
	return page, nil
}

func TestVPCDiscoveryPages(t *testing.T) {
	fake := &vpcPages{pages: []*ec2.DescribeVpcsOutput{
		{Vpcs: []types.Vpc{{VpcId: aws.String("vpc-0123456789abcdef0"), CidrBlock: aws.String("10.0.0.0/16"), Tags: []types.Tag{{Key: aws.String("Name"), Value: aws.String("demo")}, {Key: aws.String("red-hat-managed"), Value: aws.String("true")}}}}, NextToken: aws.String("next")},
		{Vpcs: []types.Vpc{{VpcId: aws.String("vpc-0123456789abcdef1")}}},
	}}
	result, err := ListVPCs(context.Background(), fake)
	if err != nil || result.Total != 2 || result.Size != 2 || fake.calls != 2 || result.Items[0].Name != "demo" || !result.Items[0].RedHatManaged {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestVPCDiscoveryEmptyAndMalformed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		output  *ec2.DescribeVpcsOutput
		invalid bool
	}{
		{"empty", &ec2.DescribeVpcsOutput{}, false},
		{"nil response", nil, true},
		{"missing ID", &ec2.DescribeVpcsOutput{Vpcs: []types.Vpc{{}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ListVPCs(context.Background(), &vpcPages{pages: []*ec2.DescribeVpcsOutput{tc.output}})
			if tc.invalid {
				var failure *customeraws.Error
				if !errors.As(err, &failure) || failure.Kind != customeraws.InvalidResponse || result != nil {
					t.Fatalf("result=%v err=%v", result, err)
				}
			} else if err != nil || result.Items == nil || result.Total != 0 {
				t.Fatalf("result=%v err=%v", result, err)
			}
		})
	}
}
