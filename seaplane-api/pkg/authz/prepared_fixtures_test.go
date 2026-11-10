package authz

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	accountID       = "123456789012"
	region          = "us-east-1"
	roleARN         = "arn:aws:iam::123456789012:role/platform/readers"
	sessionARN      = "arn:aws:sts::123456789012:assumed-role/readers/session-a"
	otherSessionARN = "arn:aws:sts::123456789012:assumed-role/readers/session-b"
	userARN         = "arn:aws:iam::123456789012:user/alice"
	readPermit      = `permit(principal, action in Seaplane::Action::"ReadOnly", resource);`
	labelPermit     = `@description("stored labels, not spec tags")
permit(principal, action == Seaplane::Action::"DescribeCluster", resource is Seaplane::Cluster)
when { resource.hasTag("example.com/team") && resource.getTag("example.com/team") == "blue \"team\"" }
unless { resource.hasTag("example.com/blocked") };`
)

type preparedFixture struct {
	store    *Store
	records  []ResolvedAttachment
	evaluate evaluator
}

func (f *preparedFixture) Prepare(ctx context.Context, id Identity, req RequestContext) (*Prepared, error) {
	p, err := f.store.prepareResolved(ctx, id, req, f.records)
	if p != nil && f.evaluate != nil {
		p.evaluate = f.evaluate
	}
	return p, err
}
func resolvedFixture(t *testing.T, serviceRegion string, records []ResolvedAttachment) *preparedFixture {
	t.Helper()
	model, validator, err := loadSchema()
	if err != nil {
		t.Fatal(err)
	}
	return &preparedFixture{store: &Store{config: StoreConfig{Region: serviceRegion}, model: model, validator: validator}, records: records}
}
func readFixture() []ResolvedAttachment {
	return grantRecords([]grant{{id: "role-read", policy: "read", content: readPermit, principal: roleARN}})
}
func testIdentity(arn string) Identity {
	return Identity{AccountID: accountID, CallerARN: arn}
}

func testRequestContext() RequestContext {
	return RequestContext{SourceIP: "192.0.2.1", UserAgent: "authz-test", RequestTime: time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)}
}

func testCluster() Resource {
	return Resource{Kind: Cluster, ID: "4610b27e-cf07-4a18-b1a1-7e8f52e95701", AccountID: accountID, Labels: map[string]string{"example.com/team": `blue "team"`}}
}

func testCollection() Resource {
	return Resource{Kind: Collection, CollectionKind: Cluster, AccountID: accountID}
}

func checkFailure(t *testing.T, err error, stage Stage) *Failure {
	t.Helper()
	var failure *Failure
	if !errors.As(err, &failure) || failure.Stage != stage || failure.Err == nil || err.Error() != "authorization failed" {
		t.Fatalf("want safe %s failure, got %v (%+v)", stage, err, failure)
	}
	return failure
}

// decide prepares one request and checks a single action.
func decide(t *testing.T, a Resolver, id Identity, action Action, resource Resource) Decision {
	t.Helper()
	p, err := a.Prepare(t.Context(), id, testRequestContext())
	if err != nil {
		t.Fatal(err)
	}
	d, err := p.Check(t.Context(), action, resource)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
