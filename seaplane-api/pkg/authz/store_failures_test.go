//go:build integration

package authz_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

type storeTransport func(*http.Request) (*http.Response, error)

func (send storeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return send(request)
}

func interceptedStore(t *testing.T, client *dynamodb.Client, cfg authz.StoreConfig, send storeTransport) *authz.Store {
	t.Helper()
	wrapped := dynamodb.New(client.Options(), func(o *dynamodb.Options) { o.HTTPClient = &http.Client{Transport: send} })
	store, err := authz.NewStore(wrapped, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStoredReadFailures(t *testing.T) {
	store, _, client, cfg := localStores(t)
	ctx := t.Context()
	policy, err := store.CreatePolicy(ctx, "read", `permit(principal, action, resource);`)
	if err != nil {
		t.Fatal(err)
	}
	caller := "arn:aws:iam::111111111111:user/alice"
	if _, err := store.CreateAttachment(ctx, authz.AttachmentSpec{PrincipalARN: caller, Policy: authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}, Scope: authz.GlobalScope}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"batch-unavailable", "regional-unavailable", "unprocessed-once", "unprocessed-always", "unprocessed-canceled"} {
		t.Run(mode, func(t *testing.T) {
			attempts := 0
			var lastAttempt time.Time
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wrapped := interceptedStore(t, client, cfg, func(request *http.Request) (*http.Response, error) {
				target := request.Header.Get("X-Amz-Target")
				if strings.HasSuffix(target, ".BatchGetItem") {
					attempts++
					if !lastAttempt.IsZero() {
						minimum := 100 * time.Millisecond * time.Duration(1<<(attempts-2))
						if time.Since(lastAttempt) < minimum {
							t.Errorf("batch retry %d occurred before backoff %s", attempts, minimum)
						}
					}
					lastAttempt = time.Now()
					if mode == "unprocessed-canceled" && attempts == 1 {
						timer := time.AfterFunc(20*time.Millisecond, cancel)
						t.Cleanup(func() { timer.Stop() })
					}
					if mode == "batch-unavailable" {
						return nil, errors.New("injected unavailable policy store")
					}
					if mode == "unprocessed-always" || mode == "unprocessed-canceled" || (mode == "unprocessed-once" && attempts == 1) {
						var body struct{ RequestItems json.RawMessage }
						if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
							return nil, err
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/x-amz-json-1.0"}}, Body: io.NopCloser(strings.NewReader(`{"Responses":{},"UnprocessedKeys":` + string(body.RequestItems) + `}`))}, nil
					}
				}
				if mode == "regional-unavailable" && strings.HasSuffix(target, ".Query") {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					request.Body = io.NopCloser(bytes.NewReader(body))
					if bytes.Contains(body, []byte(cfg.RegionalTable)) {
						return nil, errors.New("injected regional store failure")
					}
				}
				return http.DefaultTransport.RoundTrip(request)
			})
			resolved, err := wrapped.Resolve(ctx, authz.Identity{AccountID: account, CallerARN: caller})
			if mode == "unprocessed-canceled" {
				if !errors.Is(err, context.Canceled) || resolved != nil || attempts != 1 {
					t.Fatalf("canceled retry = %+v, %v, attempts %d", resolved, err, attempts)
				}
			} else if mode == "unprocessed-once" {
				if err != nil || len(resolved) != 1 || attempts != 2 {
					t.Fatalf("retry = %+v, %v, attempts %d", resolved, err, attempts)
				}
			} else if err == nil || resolved != nil {
				t.Fatalf("partial failure result = %+v, %v", resolved, err)
			}
			if mode == "unprocessed-always" && attempts != 4 {
				t.Fatalf("batch attempts = %d, want 4", attempts)
			}
		})
	}
	for _, id := range []authz.Identity{{AccountID: "222222222222", CallerARN: "arn:aws:iam::222222222222:user/alice"}, {AccountID: account, CallerARN: caller, IsAllowlisted: true}} {
		if resolved, err := store.Resolve(ctx, id); err == nil || resolved != nil {
			t.Fatalf("identity bypass = %+v, %v", resolved, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if resolved, err := store.Resolve(canceled, authz.Identity{AccountID: account, CallerARN: caller}); err == nil || resolved != nil {
		t.Fatalf("canceled result = %+v, %v", resolved, err)
	}
}

func TestStoredTransactionRollback(t *testing.T) {
	store, _, client, cfg := localStores(t)
	policy, err := store.CreatePolicy(t.Context(), "read", `permit(principal, action, resource);`)
	if err != nil {
		t.Fatal(err)
	}
	caller := "arn:aws:iam::111111111111:user/alice"
	binding, err := store.CreateAttachment(t.Context(), authz.AttachmentSpec{PrincipalARN: caller, Policy: authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}, Scope: authz.GlobalScope})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := interceptedStore(t, client, cfg, func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.Header.Get("X-Amz-Target"), ".TransactWriteItems") {
			// Change the referenced policy after validation but before the transaction.
			if _, err := store.UpdatePolicy(t.Context(), policy.ID, policy.Revision, `forbid(principal, action, resource);`); err != nil {
				return nil, err
			}
		}
		return http.DefaultTransport.RoundTrip(request)
	})
	if _, err := wrapped.UpdateAttachment(t.Context(), binding.Scope, binding.ID, binding.Revision, binding.Policy); !errors.Is(err, authz.ErrConflict) {
		t.Fatalf("racing policy mutation = %v", err)
	}
	got, err := store.GetAttachment(t.Context(), binding.Scope, binding.ID)
	if err != nil || got != binding {
		t.Fatalf("primary row changed despite rollback: %+v, %v", got, err)
	}
	resolved, err := store.Resolve(t.Context(), authz.Identity{AccountID: account, CallerARN: caller})
	if err != nil || len(resolved) != 1 || resolved[0].Attachment != binding {
		t.Fatalf("lookup row changed despite rollback: %+v, %v", resolved, err)
	}
}

func TestStoredPolicyRepairAfterSchemaDrift(t *testing.T) {
	store, _, client, cfg := localStores(t)
	ctx := t.Context()
	policy, err := store.CreatePolicy(ctx, "drifted", `permit(principal, action, resource);`)
	if err != nil {
		t.Fatal(err)
	}
	caller := "arn:aws:iam::111111111111:user/alice"
	ref := authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}
	if _, err := store.CreateAttachment(ctx, authz.AttachmentSpec{PrincipalARN: caller, Policy: ref, Scope: authz.GlobalScope}); err != nil {
		t.Fatal(err)
	}

	// Simulate a schema change that invalidates stored content without a new write.
	policy.Content = `permit(principal, action == Seaplane::Action::"RemovedAction", resource);`
	payload, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{TableName: &cfg.PolicyTable, Item: map[string]types.AttributeValue{
		"pk":       &types.AttributeValueMemberS{Value: "ACCOUNT#" + account},
		"sk":       &types.AttributeValueMemberS{Value: "POLICY#" + policy.ID},
		"revision": &types.AttributeValueMemberS{Value: policy.Revision},
		"record":   &types.AttributeValueMemberS{Value: string(payload)},
	}})
	if err != nil {
		t.Fatal(err)
	}

	// Management reads still expose the record and revision needed for repair.
	if policies, err := store.ListPolicies(ctx); err != nil || len(policies) != 1 {
		t.Fatalf("list = %+v, %v", policies, err)
	}
	got, err := store.GetPolicy(ctx, ref)
	if err != nil || got != policy {
		t.Fatalf("get = %+v, %v", got, err)
	}

	// Evaluation and new attachments still reject the drifted content.
	if prepared, err := store.Prepare(ctx, authz.Identity{AccountID: account, CallerARN: caller}, authz.RequestContext{RequestTime: time.Now()}); err == nil || prepared != nil {
		t.Fatalf("drifted policy evaluated: %v", err)
	}
	if _, err := store.CreateAttachment(ctx, authz.AttachmentSpec{PrincipalARN: "arn:aws:iam::111111111111:user/bob", Policy: ref, Scope: authz.GlobalScope}); err == nil {
		t.Fatal("drifted policy attached")
	}
	if err := store.DeletePolicy(ctx, policy.ID, got.Revision); err != nil {
		t.Fatal(err)
	}
}
