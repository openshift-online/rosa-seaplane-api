//go:build integration

package authz_test

import (
	"errors"
	"maps"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// managementMetrics points authz.DefaultMetrics at a fresh registry for one test.
func managementMetrics(t *testing.T) *prometheus.Registry {
	t.Helper()
	registry := prometheus.NewRegistry()
	metrics, err := authz.NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	original := authz.DefaultMetrics
	authz.DefaultMetrics = metrics
	t.Cleanup(func() { authz.DefaultMetrics = original })
	return registry
}

// sample returns the series with exactly these labels, or an empty metric.
func sample(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			got := map[string]string{}
			for _, label := range metric.Label {
				got[label.GetName()] = label.GetValue()
			}
			if maps.Equal(got, labels) {
				return metric
			}
		}
	}
	return &dto.Metric{}
}

// TestManagementMetrics records one outcome and duration per management call,
// including the DynamoDB grant read, and a failure stage for errors.
func TestManagementMetrics(t *testing.T) {
	m, _, admin, delegate, _ := managementFixture(t)
	registry := managementMetrics(t)
	operation := string(authz.ListPolicies)
	outcome := func(o authz.Outcome) map[string]string {
		return map[string]string{"operation": operation, "outcome": string(o)}
	}

	if _, err := m.ListPolicies(t.Context(), admin); err != nil {
		t.Fatal(err)
	}
	_, err := m.ListPolicies(t.Context(), delegate)
	if denial, ok := errors.AsType[*authz.Denial](err); !ok || !errors.Is(err, authz.ErrManagementDenied) || denial.Action != authz.ListPolicies {
		t.Fatalf("ungranted delegate = %#v", err)
	}
	policy := grantManagement(t, m, admin, delegate, "broken", `permit(principal, action, resource);`, authz.GlobalScope)
	if _, err := m.DeletePolicy(t.Context(), admin, authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}, policy.Revision); err != nil {
		t.Fatal(err)
	}
	_, err = m.ListPolicies(t.Context(), delegate)
	failure, isFailure := errors.AsType[*authz.Failure](err)
	if !isFailure || failure.Action != authz.ListPolicies {
		t.Fatalf("missing policy = %#v", err)
	}

	for _, o := range []authz.Outcome{authz.OutcomeAllow, authz.OutcomeDeny, authz.OutcomeError} {
		if got := sample(t, registry, "authz_requests_total", outcome(o)).GetCounter().GetValue(); got != 1 {
			t.Errorf("%s requests = %v, want 1", o, got)
		}
		if got := sample(t, registry, "authz_duration_seconds", outcome(o)).GetHistogram().GetSampleCount(); got != 1 {
			t.Errorf("%s duration samples = %v, want 1", o, got)
		}
	}
	stage := map[string]string{"operation": operation, "stage": string(failure.Stage)}
	if got := sample(t, registry, "authz_failures_total", stage).GetCounter().GetValue(); got != 1 {
		t.Errorf("failures %v = %v, want 1", stage, got)
	}
}
