package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	v1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/internal/authztest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPersistentResolutionFailsClosed(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"__type":"InternalServerError","message":"stored-policy-secret"}`)
	}))
	defer endpoint.Close()
	client := dynamodb.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""), RetryMaxAttempts: 1}, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint.URL) })
	t.Run("read failure", func(t *testing.T) {
		cfg := config.NewConfig()
		cfg.Regional.AWSRegion = "us-east-1"
		management, err := authz.NewService(client, authz.ServiceConfig{PolicyTable: "policies", GlobalTable: "global", RegionalTable: "regional"}, "us-east-1")
		if err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logs, nil))
		scheme := runtime.NewScheme()
		if err := corev1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		db := seaplanedb.NewClientFrom(fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "accounts", Name: resourceTestAccount}}).Build(), logger)
		srv, err := New(cfg, db, management, logger)
		if err != nil {
			t.Fatal(err)
		}
		before := authzMetricSamples(t)
		w := resourceRequest(srv, "GET", "clusters", "")
		if w.Code != 500 || !strings.Contains(w.Body.String(), "AUTHZ-FAILED-001") || strings.Contains(w.Body.String(), "stored-policy-secret") {
			t.Fatalf("unsafe failure: %d %s", w.Code, w.Body)
		}
		assertAuthzOutcome(t, before, authzMetricSamples(t), "ListClusters", "error")
		if !strings.Contains(logs.String(), `"stage":"resolution"`) || strings.Contains(logs.String(), "bundle_hash") {
			t.Fatalf("incorrect source/stage: %s", &logs)
		}
	})
}

func TestPersistentDanglingPolicy(t *testing.T) {
	objects := resourceFixtures()
	writes := 0
	srv, db, management := resourceServerWithManagement(t, []string{actionPermit("UpdateCluster")}, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		writes++
		return c.Update(ctx, obj, opts...)
	}}, objects...)
	admin := authztest.Admin(resourceTestAccount)
	policy, err := management.CreatePolicy(t.Context(), admin, "stored-policy-secret", actionPermit("DescribeCluster"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := management.CreateAttachment(t.Context(), admin, authz.AttachmentSpec{Policy: authz.PolicyRef{ID: policy.ID, Kind: authz.CustomPolicy}, PrincipalARN: "arn:aws:iam::" + resourceTestAccount + ":user/test", Scope: authz.RegionalScope}); err != nil {
		t.Fatal(err)
	}
	const body = `{"spec":{"properties":{"change":"requested"}}}`
	if w := resourceRequest(srv, "PUT", "clusters/parent", body); w.Code != 200 || writes != 1 {
		t.Fatalf("intact grant did not write: status=%d writes=%d body=%s", w.Code, writes, w.Body)
	}
	key := client.ObjectKeyFromObject(objects[0])
	var saved v1.Cluster
	if err := db.Get(t.Context(), key, &saved); err != nil {
		t.Fatal(err)
	}
	// A valid global permit must not hide a dangling policy required by the later regional scope.
	if _, err := management.DeletePolicy(t.Context(), admin, authz.PolicyRef{ID: policy.ID, Kind: authz.CustomPolicy}, policy.Revision); err != nil {
		t.Fatal(err)
	}
	before := authzMetricSamples(t)
	w := resourceRequest(srv, "PUT", "clusters/parent", body)
	var status metav1.Status
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || w.Code != 500 || status.Code != 500 || status.Kind != "Status" || status.Message != "AUTHZ-FAILED-001: Authorization failed" || status.Details != nil || strings.Contains(w.Body.String(), policy.ID) || writes != 1 {
		t.Fatalf("unsafe dangling policy response: status=%d writes=%d body=%s err=%v", w.Code, writes, w.Body, err)
	}
	after := authzMetricSamples(t)
	assertAuthzOutcome(t, before, after, "UpdateCluster", "error")
	keyMetric := "authz_failures_total/operation=UpdateCluster/stage=resolution"
	if delta := after[keyMetric] - before[keyMetric]; delta != 1 {
		t.Fatalf("resolution failure delta=%v, want 1", delta)
	}
	var unchanged v1.Cluster
	if err := db.Get(t.Context(), key, &unchanged); err != nil || unchanged.ResourceVersion != saved.ResourceVersion {
		t.Fatalf("failed resolution changed resource: version=%q want=%q err=%v", unchanged.ResourceVersion, saved.ResourceVersion, err)
	}
}

func TestEnrollmentLookupFailureFailsClosed(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			shouldFailEnrollment := false
			enrollmentReads, resourceReads, writes := 0, 0, 0
			hooks := interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, isConfigMap := obj.(*corev1.ConfigMap); isConfigMap && key == (client.ObjectKey{Namespace: "accounts", Name: resourceTestAccount}) {
						enrollmentReads++
						if shouldFailEnrollment {
							return errors.New("enrollment-db-secret")
						}
						return c.Get(ctx, key, obj, opts...)
					}
					resourceReads++
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					resourceReads++
					return c.List(ctx, list, opts...)
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					writes++
					return c.Create(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					writes++
					return c.Update(ctx, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					writes++
					return c.Delete(ctx, obj, opts...)
				},
			}
			srv, _ := resourceServer(t, []string{actionPermit("DescribeCluster"), actionPermit("UpdateCluster")}, hooks, resourceFixtures()...)
			body := ""
			if method == http.MethodPut {
				body = `{"spec":{"properties":{"change":"requested"}}}`
			}
			before := authzMetricSamples(t)
			w := resourceRequest(srv, method, "clusters/parent", body)
			if w.Code != http.StatusOK || enrollmentReads != 1 || resourceReads != 1 {
				t.Fatalf("enrolled and granted caller: status=%d enrollmentReads=%d resourceReads=%d body=%s", w.Code, enrollmentReads, resourceReads, w.Body)
			}
			action := "DescribeCluster"
			wantWrites := 0
			if method == http.MethodPut {
				action, wantWrites = "UpdateCluster", 1
			}
			assertAuthzOutcome(t, before, authzMetricSamples(t), action, "allow")
			if writes != wantWrites {
				t.Fatalf("baseline writes=%d, want %d", writes, wantWrites)
			}

			shouldFailEnrollment = true
			enrollmentReads, resourceReads, writes = 0, 0, 0
			before = authzMetricSamples(t)
			w = resourceRequest(srv, method, "clusters/parent", body)
			var status metav1.Status
			if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusForbidden || status.Kind != "Status" || status.Status != metav1.StatusFailure || status.Code != http.StatusForbidden || status.Reason != metav1.StatusReasonForbidden || status.Message != "AUTH-002: Account is not registered" || status.Details != nil || strings.Contains(w.Body.String(), "enrollment-db-secret") {
				t.Fatalf("unsafe enrollment lookup failure: status=%d body=%s", w.Code, w.Body)
			}
			if enrollmentReads != 1 || resourceReads != 0 || writes != 0 {
				t.Fatalf("lookup failure crossed admission: enrollmentReads=%d resourceReads=%d writes=%d", enrollmentReads, resourceReads, writes)
			}
			if !reflect.DeepEqual(before, authzMetricSamples(t)) {
				t.Fatal("enrollment lookup failure counted as an authorization attempt")
			}
		})
	}
}
