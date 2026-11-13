package authz

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cedar "github.com/cedar-policy/cedar-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

func metricSample(t *testing.T, r prometheus.Gatherer, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	families, err := r.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			match := len(metric.Label) == len(labels)
			for _, label := range metric.Label {
				if labels[label.GetName()] != label.GetValue() {
					match = false
				}
			}
			if match {
				return metric
			}
		}
	}
	return &dto.Metric{}
}

func newTestMetrics(t *testing.T) (*Metrics, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewMetrics(registry); err == nil {
		t.Fatal("duplicate registration not reported")
	}
	return metrics, registry
}

func TestMetricsAccounting(t *testing.T) {
	metrics, registry := newTestMetrics(t)
	for _, tc := range []struct {
		outcome Outcome
		stage   Stage
	}{
		{OutcomeAllow, StageNone}, {OutcomeDeny, StageNone}, {OutcomeError, StageEvaluation},
	} {
		t.Run(string(tc.outcome)+" "+string(tc.stage), func(t *testing.T) {
			labels := map[string]string{"operation": string(DescribeCluster), "outcome": string(tc.outcome)}
			failureLabels := map[string]string{"operation": string(DescribeCluster), "stage": string(tc.stage)}
			before := metricSample(t, registry, "authz_requests_total", labels).GetCounter().GetValue()
			beforeSamples := metricSample(t, registry, "authz_duration_seconds", labels).GetHistogram().GetSampleCount()
			beforeFailures := metricSample(t, registry, "authz_failures_total", failureLabels).GetCounter().GetValue()

			attempt := metrics.Start(DescribeCluster)
			attempt.started = time.Now().Add(-52 * time.Millisecond)
			attempt.Finish(tc.outcome, tc.stage)
			attempt.Finish(OutcomeDeny, StageNone)

			histogram := metricSample(t, registry, "authz_duration_seconds", labels).GetHistogram()
			if metricSample(t, registry, "authz_requests_total", labels).GetCounter().GetValue()-before != 1 || histogram.GetSampleCount()-beforeSamples != 1 || histogram.GetSampleSum() < 0.05 {
				t.Fatalf("bad sample delta: %+v", histogram)
			}
			if len(histogram.Bucket) != 9 || histogram.Bucket[0].GetUpperBound() != 0.001 || histogram.Bucket[8].GetUpperBound() != 1 {
				t.Fatalf("unexpected buckets: %+v", histogram.Bucket)
			}
			wantFailure := 0.0
			if tc.outcome == OutcomeError {
				wantFailure = 1
			}
			if delta := metricSample(t, registry, "authz_failures_total", failureLabels).GetCounter().GetValue() - beforeFailures; delta != wantFailure {
				t.Fatalf("failure delta %v", delta)
			}
		})
	}
}

// Checks emit nothing; only the request-level attempt is counted, and a late
// evaluation failure turns the whole list into one error.
func TestMetricsPerRequest(t *testing.T) {
	metrics, registry := newTestMetrics(t)
	a := resolvedFixture(t, region, readFixture())
	attempt := metrics.Start(ListClusters)
	p, err := a.Prepare(t.Context(), testIdentity(sessionARN), testRequestContext())
	if err != nil {
		t.Fatal(err)
	}
	if d, err := p.Check(t.Context(), ListClusters, testCollection()); err != nil || !d.Allowed {
		t.Fatalf("collection: %+v %v", d, err)
	}
	for range 10 {
		if d, err := p.Check(t.Context(), DescribeCluster, testCluster()); err != nil || !d.Allowed {
			t.Fatalf("item: %+v %v", d, err)
		}
	}
	if families, err := registry.Gather(); err != nil || len(families) != 0 {
		t.Fatalf("checks emitted metrics: %+v %v", families, err)
	}
	p.evaluate = func(*cedar.PolicySet, cedar.EntityMap, cedar.Request) (cedar.Decision, cedar.Diagnostic, error) {
		return cedar.Allow, cedar.Diagnostic{}, errors.New("late failure")
	}
	_, err = p.Check(t.Context(), DescribeCluster, testCluster())
	attempt.Finish(OutcomeError, checkFailure(t, err, StageEvaluation).Stage)
	attempt.Finish(OutcomeAllow, StageNone)

	errorLabels := map[string]string{"operation": string(ListClusters), "outcome": string(OutcomeError)}
	allowLabels := map[string]string{"operation": string(ListClusters), "outcome": string(OutcomeAllow)}
	if metricSample(t, registry, "authz_requests_total", errorLabels).GetCounter().GetValue() != 1 || metricSample(t, registry, "authz_requests_total", allowLabels).GetCounter().GetValue() != 0 || metricSample(t, registry, "authz_failures_total", map[string]string{"operation": string(ListClusters), "stage": string(StageEvaluation)}).GetCounter().GetValue() != 1 {
		t.Fatal("late failure did not count exactly once as error")
	}
}

func TestDefaultMetricsExposed(t *testing.T) {
	labels := map[string]string{"operation": string(DescribeCluster), "outcome": string(OutcomeDeny)}
	before := metricSample(t, prometheus.DefaultGatherer, "authz_requests_total", labels).GetCounter().GetValue()
	DefaultMetrics.Start(DescribeCluster).Finish(OutcomeDeny, StageNone)
	if metricSample(t, prometheus.DefaultGatherer, "authz_requests_total", labels).GetCounter().GetValue()-before != 1 {
		t.Fatal("default registry did not receive the sample")
	}
	recorder := httptest.NewRecorder()
	promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}).ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(recorder.Body.String(), `authz_duration_seconds_bucket{operation="DescribeCluster",outcome="deny",le="+Inf"}`) {
		t.Fatalf("missing +Inf bucket in exposition")
	}
}
