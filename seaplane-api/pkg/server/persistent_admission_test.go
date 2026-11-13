package server

import (
	"io"
	"log/slog"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/internal/authztest"

	v1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRCGrantRequiresEnrollment(t *testing.T) {
	cfg := config.NewConfig()
	cfg.Regional.AccountID = resourceTestAccount
	cfg.Regional.AWSRegion = "us-east-1"
	management := authztest.Management(t, resourceTestAccount, "us-east-1")
	admin := authztest.Admin(resourceTestAccount)
	if _, err := management.CreatePolicy(t.Context(), admin, "read", actionPermit("ListClusters")); err != nil {
		t.Fatal(err)
	}
	attachment, err := management.CreateAttachment(t.Context(), admin, authz.AttachmentSpec{Policy: authz.PolicyRef{ID: "read", Kind: authz.CustomPolicy}, PrincipalARN: "arn:aws:iam::" + resourceTestAccount + ":user/test", Scope: authz.RegionalScope})
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := seaplanedb.NewClientFrom(fake.NewClientBuilder().WithScheme(scheme).WithObjects(resourceFixtures()...).Build(), logger)
	srv, err := New(cfg, db, management, logger)
	if err != nil {
		t.Fatal(err)
	}
	if w := resourceRequest(srv, "GET", "clusters", ""); w.Code != 403 {
		t.Fatalf("stored grant bypassed enrollment: %d %s", w.Code, w.Body)
	}
	if err := db.EnrollAccount(t.Context(), resourceTestAccount); err != nil {
		t.Fatal(err)
	}
	before := authzMetricSamples(t)
	w := resourceRequest(srv, "GET", "clusters", "")
	if w.Code != 200 {
		t.Fatalf("enrollment plus stored grant did not serve RC account: %d %s", w.Code, w.Body)
	}
	assertAuthzOutcome(t, before, authzMetricSamples(t), "ListClusters", "allow")
	if _, err := management.DeleteAttachment(t.Context(), admin, attachment.Scope, attachment.ID, attachment.Revision); err != nil {
		t.Fatal(err)
	}
	before = authzMetricSamples(t)
	w = resourceRequest(srv, "GET", "clusters", "")
	if w.Code != 403 {
		t.Fatalf("enrollment bypassed revoked grant: %d %s", w.Code, w.Body)
	}
	assertAuthzOutcome(t, before, authzMetricSamples(t), "ListClusters", "deny")
}
