package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/customeraws"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/discovery"
)

type inquiryTransport func(*http.Request) (*http.Response, error)

func (f inquiryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type inquiryFactory struct {
	calls   int
	failure error
}

func (f *inquiryFactory) Get(ctx context.Context, role, account string) (*customeraws.Client, error) {
	f.calls++
	if role != "arn:aws:iam::"+testAccountID+":role/Installer" || account != testAccountID {
		return nil, io.ErrUnexpectedEOF
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, io.ErrUnexpectedEOF
	}
	if f.failure != nil {
		return nil, f.failure
	}
	return &customeraws.Client{AccountID: account, SessionARN: "arn:aws:sts::" + account + ":assumed-role/Installer/demo", Config: aws.Config{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, HTTPClient: &http.Client{Transport: inquiryTransport(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(string(body), "Action=DescribeVpcs") {
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/xml"}}, Body: io.NopCloser(strings.NewReader(`<DescribeVpcsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><vpcSet><item><vpcId>vpc-0123456789abcdef0</vpcId><cidrBlock>10.0.0.0/16</cidrBlock></item></vpcSet></DescribeVpcsResponse>`))}, nil
	})}}}, nil
}

func TestAWSVPCInquiry(t *testing.T) {
	const valid = `{"installerRoleArn":"arn:aws:iam::123456789012:role/Installer"}`
	for _, tc := range []struct {
		name, policy, body string
		failure            error
		status, calls      int
	}{
		{"allowed", clusterReadPermit, valid, nil, 200, 1},
		{"denied", clusterListPermit, valid, nil, 403, 0},
		{"missing role", clusterReadPermit, `{}`, nil, 400, 0},
		{"foreign role", clusterReadPermit, `{"installerRoleArn":"arn:aws:iam::999999999999:role/Installer"}`, nil, 400, 0},
		{"unknown field", clusterReadPermit, `{"installerRoleArn":"arn:aws:iam::123456789012:role/Installer","region":"us-west-2"}`, nil, 400, 0},
		{"trailing JSON", clusterReadPermit, valid + ` {}`, nil, 400, 0},
		{"trust failure", clusterReadPermit, valid, &customeraws.Error{Kind: customeraws.TrustFailed}, 422, 1},
		{"permission failure", clusterReadPermit, valid, &customeraws.Error{Kind: customeraws.PermissionDenied}, 422, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory := &inquiryFactory{failure: tc.failure}
			h := NewAWSInquiriesHandler(factory, clusterAuthorizer(t, tc.policy), "us-east-1", slog.New(slog.NewTextHandler(io.Discard, nil)))
			request := httptest.NewRequest(http.MethodPost, "/api/v0/aws_inquiries/vpcs", strings.NewReader(tc.body)).WithContext(testContext(testAccountID))
			response := httptest.NewRecorder()
			h.ListVPCs(response, request)
			if response.Code != tc.status || factory.calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, factory.calls, response.Body.String())
			}
			if response.Code == 200 {
				var list discovery.VPCList
				if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil || list.Total != 1 || list.Items[0].ID != "vpc-0123456789abcdef0" {
					t.Fatalf("bad VPC response: %s", response.Body.String())
				}
			}
		})
	}
}
