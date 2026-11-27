package e2e_monitoring_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	awstest "github.com/openshift-online/rosa-seaplane-api/test/helpers/aws"

	"github.com/openshift-online/rosa-seaplane-api/test/helpers/thanos"
)

var _ = Describe("Observability", FlakeAttempts(2), func() {

	It("should deliver authorization allow and deny metrics to Thanos", Label("authz"), func() {
		expectAuthzDelivery("/api/v0/oidc_configs", nil, "ListOIDCConfigs", "allow", http.StatusOK)
		// Management calls are authorized against the CI-seeded e2e-policy-admin grant.
		expectAuthzDelivery("/api/v0/authz/policies", nil, "ListPolicies", "allow", http.StatusOK)
		// The e2e grants exclude requests carrying this tag (context.requestLabels), so Cedar denies the create.
		probe := map[string]any{"metadata": map[string]any{"name": "authz-deny-probe", "annotations": map[string]string{"authz-deny-probe": "true"}}}
		expectAuthzDelivery("/api/v0/dns_reservations", probe, "CreateDNSReservation", "deny", http.StatusForbidden)
	})

	It("should deliver authorization evaluation errors to Thanos", Label("authz", "authz-error"), func() {
		// The owner installs the UID-scoped fixture before explicitly opting into this test.
		cluster := os.Getenv("E2E_AUTHZ_ERROR_CLUSTER")
		if cluster == "" {
			Skip("E2E_AUTHZ_ERROR_CLUSTER not set; deployed error-series acceptance is unverified")
		}
		Expect(os.Getenv("E2E_AUTHZ_PROMETHEUS_URL")).NotTo(BeEmpty(), "regional Prometheus evidence is required for the integrated error check")
		expectAuthzDelivery("/api/v0/clusters/"+url.PathEscape(cluster), nil, "DescribeCluster", "error", http.StatusInternalServerError)
	})

	It("should have RC CloudWatch metrics in Thanos", func() {
		query := `count(aws_eks_apiserver_storage_size_bytes_maximum{cluster_type="regional-cluster"}) > 0`
		Eventually(func() bool {
			resp := thanos.Query(rhobsClient, query)
			return resp.Status == "success" && len(resp.Data.Result) > 0
		}, "1m", "15s").Should(BeTrue(),
			"Expected CloudWatch EKS metrics with cluster_type=regional-cluster in Thanos "+
				"(CW Exporter → RC Prometheus → Thanos Receive)")
	})

	It("should have MC CloudWatch metrics in Thanos via remote-write", func() {
		query := `count(aws_eks_apiserver_storage_size_bytes_maximum{cluster_type="management-cluster"}) > 0`
		Eventually(func() bool {
			resp := thanos.Query(rhobsClient, query)
			return resp.Status == "success" && len(resp.Data.Result) > 0
		}, "1m", "15s").Should(BeTrue(),
			"Expected CloudWatch EKS metrics with cluster_type=management-cluster in Thanos "+
				"(CW Exporter → MC Prometheus → remote-write → RHOBS API GW → Thanos Receive)")
	})

	It("should have Vector metrics from RC in Thanos", func() {
		query := `count(vector_component_sent_events_total{cluster_type="regional-cluster",component_type="loki"}) > 0`
		Eventually(func() bool {
			resp := thanos.Query(rhobsClient, query)
			return resp.Status == "success" && len(resp.Data.Result) > 0
		}, "1m", "15s").Should(BeTrue(),
			"Expected Vector sink metrics with cluster_type=regional-cluster in Thanos "+
				"(Vector PodMonitor → RC Prometheus → Thanos Receive)")
	})

	It("should have Vector metrics from MC in Thanos via remote-write", func() {
		query := `count(vector_component_sent_events_total{cluster_type="management-cluster",component_type="loki"}) > 0`
		Eventually(func() bool {
			resp := thanos.Query(rhobsClient, query)
			return resp.Status == "success" && len(resp.Data.Result) > 0
		}, "1m", "15s").Should(BeTrue(),
			"Expected Vector sink metrics with cluster_type=management-cluster in Thanos "+
				"(Vector PodMonitor → MC Prometheus → sigv4-proxy → RHOBS API GW → Thanos Receive)")
	})

	It("should have Loki distributor metrics in Thanos", func() {
		query := `count(loki_distributor_bytes_received_total{cluster_type="regional-cluster"}) > 0`
		Eventually(func() bool {
			resp := thanos.Query(rhobsClient, query)
			return resp.Status == "success" && len(resp.Data.Result) > 0
		}, "1m", "15s").Should(BeTrue(),
			"Expected Loki distributor metrics with cluster_type=regional-cluster in Thanos "+
				"(Loki ServiceMonitor → RC Prometheus → Thanos Receive)")
	})

})

// authzMetricValue rejects failed queries rather than mistaking them for an empty baseline.
func authzMetricValue(client *awstest.APIClient, query string) (float64, error) {
	response := thanos.Query(client, query)
	if response.Status != "success" || response.Data.ResultType != "vector" || len(response.Data.Result) != 1 || len(response.Data.Result[0].Value) != 2 {
		return 0, fmt.Errorf("expected one successful aggregate metric sample")
	}
	text, ok := response.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("expected a string metric value")
	}
	return strconv.ParseFloat(text, 64)
}

// expectAuthzDelivery requires fresh counter and duration observations after signed traffic.
// A nil body sends GET; otherwise POST.
func expectAuthzDelivery(path string, body any, operation, outcome string, code int) {
	baseURL, account := os.Getenv("E2E_BASE_URL"), os.Getenv("E2E_ACCOUNT_ID")
	Expect(baseURL).NotTo(BeEmpty(), "E2E_BASE_URL is required for authorization traffic")
	Expect(account).NotTo(BeEmpty(), "E2E_ACCOUNT_ID is required for authorization traffic")
	queries := []string{}
	for _, metric := range []string{"authz_requests_total", "authz_duration_seconds_count"} {
		queries = append(queries, fmt.Sprintf(`sum(%s{namespace="platform-api",outcome=%q,operation=%q}) or vector(0)`, metric, outcome, operation))
	}
	if outcome == "error" {
		queries = append(queries, fmt.Sprintf(`sum(authz_failures_total{namespace="platform-api",operation=%q,stage="evaluation"}) or vector(0)`, operation))
	}
	clients := map[string]*awstest.APIClient{"Thanos": rhobsClient}
	if endpoint := os.Getenv("E2E_AUTHZ_PROMETHEUS_URL"); endpoint != "" {
		clients["regional Prometheus"] = awstest.NewAPIClient(endpoint)
	}
	type baseline struct {
		client      *awstest.APIClient
		name, query string
		value       float64
	}
	baselines := []baseline{}
	for name, client := range clients {
		for _, query := range queries {
			value, err := authzMetricValue(client, query)
			Expect(err).NotTo(HaveOccurred(), "reading %s baseline: %s", name, query)
			GinkgoWriter.Printf("%s %s before=%g query=%s\n", time.Now().UTC().Format(time.RFC3339Nano), name, value, query)
			baselines = append(baselines, baseline{client, name, query, value})
		}
	}
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	response, err := awstest.NewAPIClient(baseURL).Do(method, path, body, account)
	Expect(err).NotTo(HaveOccurred())
	Expect(response.StatusCode).To(Equal(code))
	GinkgoWriter.Printf("%s %s %s status=%d operation=%s outcome=%s\n", time.Now().UTC().Format(time.RFC3339Nano), method, path, code, operation, outcome)
	if code != http.StatusOK {
		var status struct {
			Kind, Message string
			Code          int
			Details       json.RawMessage
		}
		Expect(json.Unmarshal(response.Body, &status)).To(Succeed())
		Expect(status.Kind).To(Equal("Status"))
		Expect(status.Code).To(Equal(code))
		Expect(status.Details).To(BeNil())
		message := "AUTHZ-DENIED-001: Access denied"
		if outcome == "error" {
			message = "AUTHZ-FAILED-001: Authorization failed"
		}
		Expect(status.Message).To(Equal(message))
	}
	for _, before := range baselines {
		Eventually(func() (float64, error) {
			value, err := authzMetricValue(before.client, before.query)
			GinkgoWriter.Printf("%s %s before=%g after=%g query=%s\n", time.Now().UTC().Format(time.RFC3339Nano), before.name, before.value, value, before.query)
			return value, err
		}, "5m", "15s").Should(BeNumerically(">=", before.value+1), "Expected a new authorization observation in %s: %s", before.name, before.query)
	}
}

var _ = Describe("Alerting", FlakeAttempts(2), func() {

	Context("HCP recording rules", func() {
		It("should have hcp:hostedcluster_available recording rule loaded", func() {
			Eventually(func() bool {
				return thanos.HasRule(rhobsClient, "record", "hcp:hostedcluster_available")
			}, "1m", "15s").Should(BeTrue(),
				"Expected recording rule hcp:hostedcluster_available to be loaded in Thanos Ruler")
		})

		It("should have hcp:lifecycle_installing recording rule loaded", func() {
			Eventually(func() bool {
				return thanos.HasRule(rhobsClient, "record", "hcp:lifecycle_installing")
			}, "1m", "15s").Should(BeTrue(),
				"Expected recording rule hcp:lifecycle_installing to be loaded in Thanos Ruler")
		})

		It("should have hcp:lifecycle_deleting recording rule loaded", func() {
			Eventually(func() bool {
				return thanos.HasRule(rhobsClient, "record", "hcp:lifecycle_deleting")
			}, "1m", "15s").Should(BeTrue(),
				"Expected recording rule hcp:lifecycle_deleting to be loaded in Thanos Ruler")
		})
	})

	Context("HCP SLA alerting rules", func() {
		It("should have HCPAvailabilityErrorBudgetFastBurn alert loaded", func() {
			Eventually(func() bool {
				return thanos.HasRule(rhobsClient, "alert", "HCPAvailabilityErrorBudgetFastBurn")
			}, "1m", "15s").Should(BeTrue(),
				"Expected alerting rule HCPAvailabilityErrorBudgetFastBurn to be loaded in Thanos Ruler "+
					"(14.4x burn rate, ~6min window)")
		})

		It("should have HCPAvailabilityErrorBudgetSlowBurn alert loaded", func() {
			Eventually(func() bool {
				return thanos.HasRule(rhobsClient, "alert", "HCPAvailabilityErrorBudgetSlowBurn")
			}, "1m", "15s").Should(BeTrue(),
				"Expected alerting rule HCPAvailabilityErrorBudgetSlowBurn to be loaded in Thanos Ruler "+
					"(6x burn rate, ~32min window)")
		})
	})

	Context("HCP installation alerting rules", func() {
		It("should have HCPInstallTimeout15m alert loaded", func() {
			Eventually(func() bool {
				return thanos.HasRule(rhobsClient, "alert", "HCPInstallTimeout15m")
			}, "1m", "15s").Should(BeTrue(),
				"Expected alerting rule HCPInstallTimeout15m to be loaded in Thanos Ruler")
		})
	})
})
