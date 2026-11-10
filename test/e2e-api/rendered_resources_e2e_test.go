package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// renderedResourceInList checks all pages because the rendered-resource GET
// handlers also look up the source object. After the source is deleted, a GET
// can return 404 even while the rendered snapshot is still present.
func renderedResourceInList(apiClient *APIClient, path, resourceName, accountID string) (bool, error) {
	const pageSize = 100

	for offset := 0; ; offset += pageSize {
		response, err := apiClient.Get(fmt.Sprintf("%s?limit=%d&offset=%d", path, pageSize, offset), accountID)
		if err != nil {
			return false, err
		}
		if response.StatusCode != http.StatusOK {
			return false, fmt.Errorf("list %s returned status %d", path, response.StatusCode)
		}

		var page struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
			Total int `json:"total"`
		}
		if err := json.Unmarshal(response.Body, &page); err != nil {
			return false, fmt.Errorf("decode list %s: %w", path, err)
		}
		for _, item := range page.Items {
			if item.Metadata.Name == resourceName {
				return true, nil
			}
		}
		if offset+len(page.Items) >= page.Total || len(page.Items) == 0 {
			return false, nil
		}
	}
}

func expectRenderedResource(apiClient *APIClient, path, resourceName, accountID, kind string) {
	attempt := 0
	Eventually(func(g Gomega) {
		attempt++
		response, err := apiClient.Get(path+"/"+resourceName, accountID)
		if err != nil {
			GinkgoWriter.Printf("[%s] rendered %s %s poll=%d request-error=true\n",
				time.Now().Format(time.RFC3339), kind, resourceName, attempt)
			g.Expect(err).NotTo(HaveOccurred())
			return
		}
		GinkgoWriter.Printf("[%s] rendered %s %s poll=%d status=%d\n",
			time.Now().Format(time.RFC3339), kind, resourceName, attempt, response.StatusCode)
		g.Expect(response.StatusCode).To(Equal(http.StatusOK))

		var rendered map[string]any
		g.Expect(json.Unmarshal(response.Body, &rendered)).To(Succeed())
		g.Expect(rendered["kind"]).To(Equal(kind))
		metadata, ok := rendered["metadata"].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(metadata["name"]).To(Equal(resourceName))
	}).WithTimeout(5*time.Minute).WithPolling(10*time.Second).Should(Succeed(),
		"rendered %s %s should be persisted", kind, resourceName)
}

func expectRenderedResourceDeleted(apiClient *APIClient, path, resourceName, accountID string) {
	attempt := 0
	Eventually(func(g Gomega) {
		attempt++
		exists, err := renderedResourceInList(apiClient, path, resourceName, accountID)
		GinkgoWriter.Printf("[%s] rendered resource deletion poll=%d path=%s name=%s exists=%t request-error=%t\n",
			time.Now().Format(time.RFC3339), attempt, path, resourceName, exists, err != nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(exists).To(BeFalse(), "rendered resource %s should be absent from %s", resourceName, path)
	}).WithTimeout(10*time.Minute).WithPolling(30*time.Second).Should(Succeed(),
		"rendered resource %s should be deleted", resourceName)
}

// deleteNodePoolAndWait waits for the source NodePool to disappear during
// deferred cleanup, before the parent Cluster cleanup runs.
func deleteNodePoolAndWait(apiClient *APIClient, accountID, nodePoolName string) {
	resp, err := apiClient.Delete("/api/v0/nodepools/"+nodePoolName, accountID)
	Expect(err).NotTo(HaveOccurred())
	if resp.StatusCode == http.StatusNotFound {
		return
	}
	Expect(resp.StatusCode).To(Equal(http.StatusAccepted), "code=%s", apiErrorCode(resp.Body))
	Eventually(func(g Gomega) {
		resp, err := apiClient.Get("/api/v0/nodepools/"+nodePoolName, accountID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(resp.StatusCode).To(Equal(http.StatusNotFound),
			"source NodePool %s should be deleted before cluster cleanup", nodePoolName)
	}).WithTimeout(10*time.Minute).WithPolling(30*time.Second).Should(Succeed(),
		"source NodePool %s should be deleted", nodePoolName)
}

var _ = Describe("Rendered resources", Ordered, Label("rendered-resources"), func() {
	var (
		baseURL, accountID string
		apiClient          *APIClient
		clusterName        string
		nodePoolName       string
		configName         string
	)

	BeforeAll(func() {
		baseURL = os.Getenv("E2E_BASE_URL")
		Expect(baseURL).NotTo(BeEmpty(), "E2E_BASE_URL must be set")
		accountID = e2eAccountID()
		apiClient = NewAPIClient(baseURL)
	})

	It("persists and removes rendered HostedCluster and NodePool resources", func() {
		config := createOidcConfig(apiClient, accountID, map[string]any{"type": "managed"})
		configID := metaUID(config)
		configName = metaName(config)
		DeferCleanup(func() { deleteOidcConfigAndWait(apiClient, accountID, configName) })

		clusterName = uniqueClusterName("r")
		response, err := apiClient.Post("/api/v0/clusters", map[string]any{
			"metadata": map[string]any{"name": clusterName},
			"spec":     minClusterSpec(configID),
		}, accountID)
		Expect(err).NotTo(HaveOccurred())
		Expect(response.StatusCode).To(Equal(http.StatusCreated), "code=%s", apiErrorCode(response.Body))
		DeferCleanup(func() { deleteClusterAndWait(apiClient, accountID, clusterName) })

		nodePoolName = clusterName + ".e2e-np"
		response, err = apiClient.Post("/api/v0/nodepools", map[string]any{
			"metadata": map[string]any{"name": nodePoolName},
			"spec":     map[string]any{"nodePool": map[string]any{"replicas": 2}},
		}, accountID)
		Expect(err).NotTo(HaveOccurred())
		Expect(response.StatusCode).To(Equal(http.StatusCreated), "code=%s", apiErrorCode(response.Body))
		DeferCleanup(func() {
			deleteNodePoolAndWait(apiClient, accountID, nodePoolName)
		})

		By("verifying the rendered HostedCluster exists")
		expectRenderedResource(apiClient, "/api/v0/renderedhostedclusters", clusterName, accountID, "HostedCluster")

		By("verifying the rendered NodePool exists")
		expectRenderedResource(apiClient, "/api/v0/renderednodepools", nodePoolName, accountID, "NodePool")

		By("deleting the NodePool and verifying its rendered snapshot is removed")
		response, err = apiClient.Delete("/api/v0/nodepools/"+nodePoolName, accountID)
		Expect(err).NotTo(HaveOccurred())
		Expect(response.StatusCode).To(Equal(http.StatusAccepted))
		expectRenderedResourceDeleted(apiClient, "/api/v0/renderednodepools", nodePoolName, accountID)

		By("deleting the Cluster and verifying its rendered snapshot is removed")
		response, err = apiClient.Delete("/api/v0/clusters/"+clusterName, accountID)
		Expect(err).NotTo(HaveOccurred())
		Expect(response.StatusCode).To(Equal(http.StatusAccepted))
		expectRenderedResourceDeleted(apiClient, "/api/v0/renderedhostedclusters", clusterName, accountID)
	})
})
