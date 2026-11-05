package e2e_test

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// See wk_dev/oidc-e2e-testing-plan.md for the scope decisions behind these tests: none of them wait
// for a real HostedCluster to reach Ready (that's e2e-sdk/e2e-cli's job); they only exercise the
// synchronous OidcConfig<->Cluster binding in seaplane-api and the operator's real reconciliation
// against real AWS (CloudFront TLS reachability, and for unmanaged, real Secrets Manager).

// defaultReleaseImage matches the fallback used by test/e2e-sdk. OCP_IMAGE
// overrides it, so CI can pin the release without editing the suite.
const defaultReleaseImage = "quay.io/openshift-release-dev/ocp-release:5.0.0-rc.5-multi"

func releaseImage() string {
	if image := os.Getenv("OCP_IMAGE"); image != "" {
		return image
	}
	return defaultReleaseImage
}

// minClusterSpec is the same minimal, VPC-free HostedCluster spec seaplane-api's own unit tests use
// (see seaplane-api/pkg/handlers/cluster_test.go's minSpec) — enough to pass Create validation
// without needing real VPC/IAM infra we don't need for OidcConfig-binding coverage.
func minClusterSpec(oidcConfigID string) map[string]any {
	return map[string]any{
		"oidcConfigId": oidcConfigID,
		"hostedCluster": map[string]any{
			"release":    map[string]any{"image": releaseImage()},
			"networking": map[string]any{},
			"platform":   map[string]any{"type": "AWS"},
		},
	}
}

// metaUID extracts metadata.uid from any decoded K8s-native response body (Cluster or OidcConfig).
func metaUID(obj map[string]any) string {
	uid, _ := oidcConfigMetadata(obj)["uid"].(string)
	return uid
}

func metaName(obj map[string]any) string {
	name, _ := oidcConfigMetadata(obj)["name"].(string)
	return name
}

// clusterLabel extracts a metadata.labels entry from a decoded Cluster response body.
func clusterLabel(cluster map[string]any, key string) string {
	labels, _ := oidcConfigMetadata(cluster)["labels"].(map[string]any)
	v, _ := labels[key].(string)
	return v
}

// issuerURL extracts spec.hostedCluster.issuerURL from a decoded Cluster response body.
func clusterIssuerURL(cluster map[string]any) string {
	spec, _ := cluster["spec"].(map[string]any)
	hc, _ := spec["hostedCluster"].(map[string]any)
	url, _ := hc["issuerURL"].(string)
	return url
}

// uniqueClusterName returns a short, unique-enough DNS-label cluster name.
func uniqueClusterName(tag string) string {
	return fmt.Sprintf("e2e%s%d", tag, time.Now().UnixMilli())
}

// createOidcConfig POSTs an OIDC config and returns its decoded body; fails the spec on error.
func createOidcConfig(apiClient *APIClient, accountID string, spec map[string]any) map[string]any {
	request := map[string]any{
		"metadata": map[string]any{"name": fmt.Sprintf("e2e-oidc-%d", time.Now().UnixNano())},
		"spec":     spec,
	}
	resp, err := apiClient.Post("/api/v0/oidc_configs", request, accountID)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusCreated), "code=%s", apiErrorCode(resp.Body))
	var created map[string]any
	Expect(json.Unmarshal(resp.Body, &created)).To(Succeed())
	return created
}

// waitForOidcConfigPhase polls GET on the config until status.phase matches want.
func waitForOidcConfigPhase(apiClient *APIClient, accountID, configName, want string, timeout time.Duration) map[string]any {
	var got map[string]any
	Eventually(func(g Gomega) {
		resp, err := apiClient.Get("/api/v0/oidc_configs/"+configName, accountID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(resp.StatusCode).To(Equal(http.StatusOK), "code=%s", apiErrorCode(resp.Body))
		g.Expect(json.Unmarshal(resp.Body, &got)).To(Succeed())
		status, _ := got["status"].(map[string]any)
		phase, _ := status["phase"].(string)
		g.Expect(phase).To(Equal(want), "oidc config %s phase", configName)
	}).WithTimeout(timeout).WithPolling(5 * time.Second).Should(Succeed())
	return got
}

// waitForClusterFinalized polls GET on the cluster until status.phase is set
func waitForClusterFinalized(apiClient *APIClient, accountID, clusterName string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		resp, err := apiClient.Get("/api/v0/clusters/"+clusterName, accountID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(resp.StatusCode).To(Equal(http.StatusOK), "code=%s", apiErrorCode(resp.Body))
		var got map[string]any
		g.Expect(json.Unmarshal(resp.Body, &got)).To(Succeed())
		status, _ := got["status"].(map[string]any)
		phase, _ := status["phase"].(string)
		g.Expect(phase).NotTo(BeEmpty(), "cluster %s should have a status.phase set by the operator", clusterName)
	}).WithTimeout(timeout).WithPolling(2 * time.Second).Should(Succeed())
}

// deleteClusterAndWait deletes a cluster and polls until it 404s; a 404 on delete itself also counts as done.
func deleteClusterAndWait(apiClient *APIClient, accountID, clusterName string) {
	resp, err := apiClient.Delete("/api/v0/clusters/"+clusterName, accountID)
	Expect(err).NotTo(HaveOccurred())
	if resp.StatusCode == http.StatusNotFound {
		return
	}
	Expect(resp.StatusCode).To(Equal(http.StatusAccepted), "code=%s", apiErrorCode(resp.Body))
	Eventually(func(g Gomega) {
		resp, err := apiClient.Get("/api/v0/clusters/"+clusterName, accountID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(resp.StatusCode).To(Equal(http.StatusNotFound), "cluster %s should be gone", clusterName)
	}).WithTimeout(10 * time.Minute).WithPolling(30 * time.Second).Should(Succeed())
}

// deleteOidcConfigAndWait deletes an OIDC config and polls until it 404s; already-404 also counts as done.
func deleteOidcConfigAndWait(apiClient *APIClient, accountID, configName string) {
	resp, err := apiClient.Delete("/api/v0/oidc_configs/"+configName, accountID)
	Expect(err).NotTo(HaveOccurred())
	if resp.StatusCode == http.StatusNotFound {
		return
	}
	Expect(resp.StatusCode).To(Equal(http.StatusAccepted), "code=%s", apiErrorCode(resp.Body))
	Eventually(func(g Gomega) {
		resp, err := apiClient.Get("/api/v0/oidc_configs/"+configName, accountID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(resp.StatusCode).To(Equal(http.StatusNotFound), "oidc config %s should be gone", configName)
	}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
}

var _ = Describe("OIDC Config Lifecycle: Managed", Ordered, Label("oidcconfig", "oidc-lifecycle"), func() {
	var (
		baseURL, accountID string
		apiClient          *APIClient
	)

	BeforeAll(func() {
		baseURL = os.Getenv("E2E_BASE_URL")
		Expect(baseURL).NotTo(BeEmpty(), "E2E_BASE_URL must be set")
		accountID = e2eAccountID()
		apiClient = NewAPIClient(baseURL)
	})

	It("binds a managed config to a cluster, reaches Ready via real CloudFront TLS, then unbinds and deletes cleanly", func() {
		By("creating a managed OIDC config")
		created := createOidcConfig(apiClient, accountID, map[string]any{"type": "managed"})
		configID := metaUID(created)
		configName := metaName(created)
		Expect(configName).NotTo(BeEmpty())
		issuerURL, _ := oidcConfigSpec(created)["issuerUrl"].(string)
		Expect(issuerURL).NotTo(BeEmpty())
		DeferCleanup(func() { deleteOidcConfigAndWait(apiClient, accountID, configName) })

		By("verifying it starts Pending (AwaitingCluster) with no cluster bound yet")
		fetched := waitForOidcConfigPhase(apiClient, accountID, configName, "Pending", 30*time.Second)
		Expect(clusterLabel(fetched, "seaplane.io/cluster-namespace")).To(BeEmpty())

		By("creating a cluster referencing the config")
		clusterName := uniqueClusterName("m")
		resp, err := apiClient.Post("/api/v0/clusters", map[string]any{
			"metadata": map[string]any{"name": clusterName},
			"spec":     minClusterSpec(configID),
		}, accountID)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusCreated), "code=%s", apiErrorCode(resp.Body))
		var cluster map[string]any
		Expect(json.Unmarshal(resp.Body, &cluster)).To(Succeed())
		clusterName = metaName(cluster)
		Expect(clusterName).NotTo(BeEmpty())
		// Safety net if a later assertion fails before the explicit delete below runs.
		DeferCleanup(func() { deleteClusterAndWait(apiClient, accountID, clusterName) })
		Expect(clusterIssuerURL(cluster)).To(Equal(issuerURL), "cluster's issuerURL should be service-set from the OIDC config")

		By("verifying the config reaches Ready via a real TLS handshake against CloudFront")
		waitForOidcConfigPhase(apiClient, accountID, configName, "Ready", 2*time.Minute)

		By("deleting the cluster and confirming the claim releases")
		deleteClusterAndWait(apiClient, accountID, clusterName)
		unbound := waitForOidcConfigPhase(apiClient, accountID, configName, "Pending", 1*time.Minute)
		Expect(clusterLabel(unbound, "seaplane.io/cluster-namespace")).To(BeEmpty())
	})
})

var _ = Describe("OIDC Config Lifecycle: Reusability", Ordered, Label("oidcconfig", "oidc-lifecycle"), func() {
	var (
		baseURL, accountID string
		apiClient          *APIClient
	)

	BeforeAll(func() {
		baseURL = os.Getenv("E2E_BASE_URL")
		Expect(baseURL).NotTo(BeEmpty(), "E2E_BASE_URL must be set")
		accountID = e2eAccountID()
		apiClient = NewAPIClient(baseURL)
	})

	It("rejects a concurrent second bind, then allows sequential reuse after the first cluster releases it", func() {
		created := createOidcConfig(apiClient, accountID, map[string]any{"type": "managed"})
		configID := metaUID(created)
		configName := metaName(created)
		Expect(configName).NotTo(BeEmpty())
		DeferCleanup(func() { deleteOidcConfigAndWait(apiClient, accountID, configName) })

		By("cluster A claims the config")
		clusterAName := uniqueClusterName("a")
		respA, err := apiClient.Post("/api/v0/clusters", map[string]any{
			"metadata": map[string]any{"name": clusterAName},
			"spec":     minClusterSpec(configID),
		}, accountID)
		Expect(err).NotTo(HaveOccurred())
		Expect(respA.StatusCode).To(Equal(http.StatusCreated), "code=%s", apiErrorCode(respA.Body))
		var clusterA map[string]any
		Expect(json.Unmarshal(respA.Body, &clusterA)).To(Succeed())
		clusterAName = metaName(clusterA)
		Expect(clusterAName).NotTo(BeEmpty())
		// Safety net if a later assertion fails before the explicit delete below runs.
		DeferCleanup(func() { deleteClusterAndWait(apiClient, accountID, clusterAName) })

		// Reused for both the rejected attempt below and the successful retry after A releases the
		// claim: the rejected attempt fails before ever reaching CreateCluster (the OIDC claim check
		// runs after the name-uniqueness check but before persisting), so the name is never taken.
		nameB := uniqueClusterName("b")

		By("cluster B is rejected while A still holds the claim")
		respB, err := apiClient.Post("/api/v0/clusters", map[string]any{
			"metadata": map[string]any{"name": nameB},
			"spec":     minClusterSpec(configID),
		}, accountID)
		Expect(err).NotTo(HaveOccurred())
		Expect(respB.StatusCode).To(Equal(http.StatusConflict), "code=%s", apiErrorCode(respB.Body))
		Expect(apiErrorCode(respB.Body)).To(Equal("CLUSTERS-MGMT-CREATE-012"))

		By("waiting for cluster A's finalizer to land before deleting it")
		waitForClusterFinalized(apiClient, accountID, clusterAName, 2*time.Minute)

		By("deleting cluster A releases the claim without deleting the config")
		deleteClusterAndWait(apiClient, accountID, clusterAName)
		released := waitForOidcConfigPhase(apiClient, accountID, configName, "Pending", 1*time.Minute)
		Expect(clusterLabel(released, "seaplane.io/cluster-namespace")).To(BeEmpty())

		By("cluster B can now claim the same, still-existing config")
		respB2, err := apiClient.Post("/api/v0/clusters", map[string]any{
			"metadata": map[string]any{"name": nameB},
			"spec":     minClusterSpec(configID),
		}, accountID)
		Expect(err).NotTo(HaveOccurred())
		Expect(respB2.StatusCode).To(Equal(http.StatusCreated), "code=%s", apiErrorCode(respB2.Body))
		var clusterB map[string]any
		Expect(json.Unmarshal(respB2.Body, &clusterB)).To(Succeed())
		clusterBName := metaName(clusterB)
		Expect(clusterBName).NotTo(BeEmpty())
		DeferCleanup(func() { deleteClusterAndWait(apiClient, accountID, clusterBName) })

		By("waiting for cluster B's finalizer to land before deleting it")
		waitForClusterFinalized(apiClient, accountID, clusterBName, 2*time.Minute)
		deleteClusterAndWait(apiClient, accountID, clusterBName)
	})
})

// stsAccountID returns the AWS account ID for the given profile (empty string for the default
// profile) by shelling out to `aws sts get-caller-identity`, mirroring e2e-cli/e2e-sdk's pattern.
func stsAccountID(profile string) string {
	cmd := exec.Command("aws", "sts", "get-caller-identity", "--query", "Account", "--output", "text")
	if profile != "" {
		cmd.Env = append(os.Environ(), "AWS_PROFILE="+profile)
	}
	output, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "failed to get AWS account ID via STS (profile=%q)", profile)
	return strings.TrimSpace(string(output))
}

// Cross-account isolation is enforced by API Gateway deriving X-Amz-Account-Id from the caller's
// real SigV4 identity (see seaplane-api/pkg/middleware/identity.go), not from a client-supplied
// header. So exercising it for real requires two genuinely distinct AWS identities, matching
// e2e-cli/e2e-sdk's CUSTOMER_AWS_PROFILE convention; this Skips when that isn't configured.
var _ = Describe("OIDC Config Lifecycle: Cross-Account Isolation", Label("oidcconfig", "oidc-lifecycle"), func() {
	It("hides account A's OIDC config from account B's Get, List, and Delete", func() {
		customerProfile := os.Getenv("CUSTOMER_AWS_PROFILE")
		if customerProfile == "" {
			Skip("CUSTOMER_AWS_PROFILE is not set — cross-account isolation needs a second real AWS identity")
		}
		baseURL := os.Getenv("E2E_BASE_URL")
		Expect(baseURL).NotTo(BeEmpty(), "E2E_BASE_URL must be set")

		accountA := e2eAccountID()
		accountB := stsAccountID(customerProfile)
		apiClientA := NewAPIClient(baseURL)
		apiClientB := NewAPIClient(baseURL)
		apiClientB.AWSProfile = customerProfile

		created := createOidcConfig(apiClientA, accountA, map[string]any{"type": "managed"})
		configID := metaUID(created)
		configName := metaName(created)
		Expect(configName).NotTo(BeEmpty())
		DeferCleanup(func() { deleteOidcConfigAndWait(apiClientA, accountA, configName) })

		By("account A can see it")
		respA, err := apiClientA.Get("/api/v0/oidc_configs/"+configName, accountA)
		Expect(err).NotTo(HaveOccurred())
		Expect(respA.StatusCode).To(Equal(http.StatusOK))

		By("account B cannot Get it")
		respB, err := apiClientB.Get("/api/v0/oidc_configs/"+configName, accountB)
		Expect(err).NotTo(HaveOccurred())
		Expect(respB.StatusCode).To(Equal(http.StatusNotFound))

		By("account B does not see it in List")
		listResp, err := apiClientB.Get("/api/v0/oidc_configs", accountB)
		Expect(err).NotTo(HaveOccurred())
		Expect(listResp.StatusCode).To(Equal(http.StatusOK))
		var list struct {
			Items []map[string]any `json:"items"`
		}
		Expect(json.Unmarshal(listResp.Body, &list)).To(Succeed())
		for _, item := range list.Items {
			Expect(metaUID(item)).NotTo(Equal(configID), "account B should never see account A's config in List")
		}

		By("account B cannot Delete it either")
		delResp, err := apiClientB.Delete("/api/v0/oidc_configs/"+configName, accountB)
		Expect(err).NotTo(HaveOccurred())
		Expect(delResp.StatusCode).To(Equal(http.StatusNotFound))

		By("account A can still see it, confirming B's rejected delete was a no-op")
		respA2, err := apiClientA.Get("/api/v0/oidc_configs/"+configName, accountA)
		Expect(err).NotTo(HaveOccurred())
		Expect(respA2.StatusCode).To(Equal(http.StatusOK))
	})
})

var _ = Describe("OIDC Config Lifecycle: Unmanaged", Ordered, Label("oidcconfig", "oidc-lifecycle"), func() {
	var (
		baseURL, accountID string
		apiClient          *APIClient
		fixture            *customerOidcFixture
		smClient           *secretsmanager.Client
	)

	BeforeAll(func() {
		baseURL = os.Getenv("E2E_BASE_URL")
		Expect(baseURL).NotTo(BeEmpty(), "E2E_BASE_URL must be set")
		accountID = e2eAccountID()
		apiClient = NewAPIClient(baseURL)

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()
		var skipReason string
		fixture, skipReason = provisionCustomerOidcFixture(ctx, fmt.Sprintf("oidc-e2e-%d", time.Now().Unix()))
		if skipReason != "" {
			Skip("cannot provision self-contained customer OIDC fixture: " + skipReason)
		}
		DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer cancel()
			fixture.cleanup(cleanupCtx)
		})

		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		Expect(err).NotTo(HaveOccurred())
		smClient = secretsmanager.NewFromConfig(cfg)
	})

	It("copies the customer's signing key into local Secrets Manager and binds a cluster once Ready", func() {
		var clusterName string // set once the cluster below is created; read by the DeferCleanup below
		By("creating an unmanaged config pointing at our own reachable HTTPS host (TLS-only check) and the fixture's ARNs")
		issuerURL := strings.TrimSuffix(baseURL, "/") + fmt.Sprintf("/e2e-unmanaged-issuer-%d", time.Now().UnixNano())
		created := createOidcConfig(apiClient, accountID, map[string]any{
			"type":             "unmanaged",
			"issuerUrl":        issuerURL,
			"secretArn":        fixture.Secret,
			"installerRoleArn": fixture.RoleArn,
		})
		configID := metaUID(created)
		configName := metaName(created)
		Expect(configName).NotTo(BeEmpty())
		// Safety net: delete any bound cluster (an OIDC claim would block config deletion), then the config.
		DeferCleanup(func() {
			if clusterName != "" {
				deleteClusterAndWait(apiClient, accountID, clusterName)
			}
			deleteOidcConfigAndWait(apiClient, accountID, configName)
		})

		By("waiting for the config to reach Ready (cross-account key read + Secrets Manager copy + TLS check)")
		// Unmanaged configs must already be Ready before a cluster can bind them
		// (resolveAndClaimOidcConfig's notReady check), unlike managed.
		waitForOidcConfigPhase(apiClient, accountID, configName, "Ready", 3*time.Minute)

		By("verifying the copied key in Secrets Manager matches what we supplied")
		secretPath := oidcSigningKeySecretPath(accountID, configName)
		out, err := smClient.GetSecretValue(context.Background(), &secretsmanager.GetSecretValueInput{
			SecretId: aws.String(secretPath),
		})
		Expect(err).NotTo(HaveOccurred(), "expected StorePrivateKey to have created %s", secretPath)
		copiedKey := []byte(aws.ToString(out.SecretString))
		keysMatch := subtle.ConstantTimeCompare(copiedKey, fixture.KeyPEM) == 1
		Expect(keysMatch).To(BeTrue(), "copied Secrets Manager key does not match the fixture's signing key")

		By("creating a cluster against the now-Ready config")
		requestedClusterName := uniqueClusterName("u")
		resp, err := apiClient.Post("/api/v0/clusters", map[string]any{
			"metadata": map[string]any{"name": requestedClusterName},
			"spec":     minClusterSpec(configID),
		}, accountID)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusCreated), "code=%s", apiErrorCode(resp.Body))
		var cluster map[string]any
		Expect(json.Unmarshal(resp.Body, &cluster)).To(Succeed())
		clusterName = metaName(cluster)
		Expect(clusterName).NotTo(BeEmpty())
		Expect(clusterIssuerURL(cluster)).To(Equal(issuerURL))

		By("waiting for the cluster's finalizer to land before deleting it")
		waitForClusterFinalized(apiClient, accountID, clusterName, 2*time.Minute)

		By("deleting the cluster then the config, and confirming the Secrets Manager copy is cleaned up too")
		deleteClusterAndWait(apiClient, accountID, clusterName)
		deleteOidcConfigAndWait(apiClient, accountID, configName)
		Eventually(func(g Gomega) {
			_, err := smClient.GetSecretValue(context.Background(), &secretsmanager.GetSecretValueInput{
				SecretId: aws.String(secretPath),
			})
			var notFoundErr *smtypes.ResourceNotFoundException
			g.Expect(errors.As(err, &notFoundErr)).To(BeTrue(),
				"expected DeletePrivateKey to have removed %s, got err=%v", secretPath, err)
		}).WithTimeout(1 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	})
})

// e2eAccountID mirrors the account-resolution fallback used throughout this suite.
func e2eAccountID() string {
	accountID := os.Getenv("E2E_ACCOUNT_ID")
	if accountID != "" {
		return accountID
	}
	cmd := exec.Command("aws", "sts", "get-caller-identity", "--query", "Account", "--output", "text")
	output, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "Failed to get AWS account ID via STS")
	return strings.TrimSpace(string(output))
}
