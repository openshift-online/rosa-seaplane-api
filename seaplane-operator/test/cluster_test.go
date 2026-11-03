package integration

import (
	"encoding/json"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	dynamo "github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/dynamo"
	hd "github.com/openshift-online/rosa-seaplane-kube-applier/seaplane-dynamo/dynamodb"
)

var _ = Describe("Cluster lifecycle", func() {
	const (
		clusterName = "e2e-test-01"
		testNS      = "account-111222333444"
	)

	AfterEach(func() {
		purgeResources()
		purgeDynamoTables()
		dynamoCli.ResetCache()
	})

	It("should write correct ApplyDesires to DynamoDB when a Cluster is created", func() {
		By("creating a Cluster CR")
		cluster := newTestCluster(clusterName)
		Expect(createTestClusterWithReservation(cluster)).To(Succeed())
		managementNS := "cluster-" + string(cluster.UID)
		hostedClusterName := cluster.Spec.HostedClusterName
		Expect(hostedClusterName).NotTo(BeEmpty())
		Expect(hostedClusterName).NotTo(Equal(cluster.Name))

		By("waiting for Placement to be created and Bound")
		Eventually(func(g Gomega) {
			var p seaplanev1alpha1.Placement
			nn := types.NamespacedName{
				Namespace: testNS,
				Name:      clusterName + ".placement",
			}
			g.Expect(k8sClient.Get(ctx, nn, &p)).To(Succeed())
			g.Expect(p.Status.Phase).To(Equal(seaplanev1alpha1.PlacementPhaseBound))
			g.Expect(p.Spec.ManagementCluster).To(Equal("mc01"))
		}).Should(Succeed())

		By("waiting for ApplyDesires to appear in DynamoDB")
		specsTable := mc + "-specs-applydesires"
		Eventually(func(g Gomega) {
			items := scanTable(specsTable)
			g.Expect(len(items)).To(BeNumerically(">=", 6), "expected at least 6 ApplyDesires, got %d", len(items))
		}).Should(Succeed())

		By("verifying the 6 expected resources are present")
		items := scanTable(specsTable)
		resourceNames := map[string]bool{}
		for _, item := range items {
			name := attrString(item, "spec", "targetItem", "name")
			resource := attrString(item, "spec", "targetItem", "resource")
			resourceNames[resource+"/"+name] = true
		}

		expectedResources := []string{
			"namespaces/" + managementNS,
			"configmaps/cluster-config",
			"externalsecrets/pull-secret",
			"certificates/api-serving-cert",
			"hostedclusters/" + hostedClusterName,
			"secrets/ssh-key",
		}
		for _, expected := range expectedResources {
			Expect(resourceNames).To(HaveKey(expected), "missing resource: %s", expected)
		}

		By("verifying HostedCluster content in DynamoDB")
		var hcContent map[string]any
		for _, item := range items {
			resource := attrString(item, "spec", "targetItem", "resource")
			if resource == "hostedclusters" {
				raw := attrString(item, "spec_kubeContent")
				Expect(raw).NotTo(BeEmpty(), "kubeContent should not be empty")
				Expect(json.Unmarshal([]byte(raw), &hcContent)).To(Succeed())
				break
			}
		}
		Expect(hcContent).NotTo(BeNil(), "HostedCluster not found in DynamoDB")

		spec := hcContent["spec"].(map[string]any)
		Expect(spec["issuerURL"]).To(Equal("https://oidc.e2e.example.com/e2e-test-01"))
		Expect(spec["infraID"]).To(Equal(string(cluster.UID)))

		dns := spec["dns"].(map[string]any)
		Expect(dns["baseDomain"]).To(MatchRegexp(`^[0-9a-f]{4}\.0\.e2e\.example\.com$`))

		By("verifying ReadDesire for HostedCluster status feedback")
		readTable := mc + "-specs-readdesires"
		Eventually(func(g Gomega) {
			readItems := scanTable(readTable)
			g.Expect(readItems).ToNot(BeEmpty())
			resource := attrString(readItems[0], "spec", "targetItem", "resource")
			g.Expect(resource).To(Equal("hostedclusters"))
		}).Should(Succeed())

		By("verifying document IDs are deterministic")
		nsDocID := dynamo.NewDocumentID("seaplane-operator", "", "v1", "namespaces", "", managementNS)
		found := false
		for _, item := range items {
			if docID, ok := item["documentID"]; ok {
				if sv, ok := docID.(*dynamodbtypes.AttributeValueMemberS); ok && sv.Value == nsDocID {
					found = true
					break
				}
			}
		}
		Expect(found).To(BeTrue(), "namespace desire should have deterministic document ID %s", nsDocID)
	})

	It("should write an oidc-signing-key ExternalSecret ApplyDesire when the Cluster has OidcConfigID set", func() {
		By("creating an unmanaged OidcConfig CR referenced by the Cluster")
		oidcConfig := newTestOidcConfig()
		Expect(k8sClient.Create(ctx, oidcConfig)).To(Succeed())

		By("creating a Cluster CR with OidcConfigID set")
		cluster := newTestClusterWithOidcConfig(clusterName)
		cluster.Spec.OidcConfigID = string(oidcConfig.UID)
		Expect(createTestClusterWithReservation(cluster)).To(Succeed())

		By("waiting for Placement to be created and Bound")
		Eventually(func(g Gomega) {
			var p seaplanev1alpha1.Placement
			nn := types.NamespacedName{
				Namespace: testNS,
				Name:      clusterName + ".placement",
			}
			g.Expect(k8sClient.Get(ctx, nn, &p)).To(Succeed())
			g.Expect(p.Status.Phase).To(Equal(seaplanev1alpha1.PlacementPhaseBound))
		}).Should(Succeed())

		By("waiting for the oidc-signing-key ExternalSecret ApplyDesire to appear in DynamoDB")
		specsTable := mc + "-specs-applydesires"
		Eventually(func(g Gomega) {
			items := scanTable(specsTable)
			g.Expect(len(items)).To(BeNumerically(">=", 7), "expected at least 7 ApplyDesires, got %d", len(items))
			found := false
			for _, item := range items {
				resource := attrString(item, "spec", "targetItem", "resource")
				name := attrString(item, "spec", "targetItem", "name")
				if resource == "externalsecrets" && name == "oidc-signing-key" {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "expected an externalsecrets/oidc-signing-key ApplyDesire")
		}).Should(Succeed())

		By("verifying HostedCluster references the oidc-signing-key Secret")
		items := scanTable(specsTable)
		var hcContent map[string]any
		for _, item := range items {
			if attrString(item, "spec", "targetItem", "resource") == "hostedclusters" {
				raw := attrString(item, "spec_kubeContent")
				Expect(raw).NotTo(BeEmpty())
				Expect(json.Unmarshal([]byte(raw), &hcContent)).To(Succeed())
				break
			}
		}
		Expect(hcContent).NotTo(BeNil(), "HostedCluster not found in DynamoDB")
		spec := hcContent["spec"].(map[string]any)
		sask := spec["serviceAccountSigningKey"].(map[string]any)
		Expect(sask["name"]).To(Equal("oidc-signing-key"))
	})

	It("should propagate HostedCluster status from ReadDesire to Cluster CR", func() {
		By("creating a Cluster CR")
		cluster := newTestCluster(clusterName)
		Expect(createTestClusterWithReservation(cluster)).To(Succeed())

		By("waiting for ReadDesire to appear in DynamoDB")
		readTable := mc + "-specs-readdesires"
		var readDocID string
		Eventually(func(g Gomega) {
			items := scanTable(readTable)
			g.Expect(items).ToNot(BeEmpty())
			readDocID = attrString(items[0], "documentID")
			g.Expect(readDocID).NotTo(BeEmpty())
		}).Should(Succeed())

		By("simulating kube-applier-aws writing HostedCluster status to status-readdesires")
		hcStatus := map[string]any{
			"status": map[string]any{
				"conditions": []map[string]any{
					{
						"type":               "Available",
						"status":             "True",
						"reason":             "HostedClusterAsExpected",
						"message":            "The hosted cluster is available",
						"lastTransitionTime": "2026-06-24T00:00:00Z",
					},
				},
				"controlPlaneEndpoint": map[string]any{
					"host": "api.e2e-test.example.com",
				},
				"version": map[string]any{
					"history": []map[string]any{
						{"version": "4.17.3"},
					},
				},
			},
		}
		hcJSON, err := json.Marshal(hcStatus)
		Expect(err).NotTo(HaveOccurred())

		statusTable := mc + "-status-readdesires"
		_, err = dynamoDBCli.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(statusTable),
			Item: map[string]dynamodbtypes.AttributeValue{
				"documentID":         &dynamodbtypes.AttributeValueMemberS{Value: readDocID},
				"updateTime":         &dynamodbtypes.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
				"shard":              &dynamodbtypes.AttributeValueMemberS{Value: hd.ComputeShardDefault(readDocID)},
				"status_kubeContent": &dynamodbtypes.AttributeValueMemberS{Value: string(hcJSON)},
			},
		})
		Expect(err).NotTo(HaveOccurred())

		// Dispatch manually as belt-and-suspenders; the GSI poller will also
		// fire within a few seconds now that shard+updateTime are written.
		eventRouter.Dispatch(readDocID)

		By("verifying Cluster CR status is updated with HostedCluster data")
		Eventually(func(g Gomega) {
			var c seaplanev1alpha1.Cluster
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &c)).To(Succeed())
			g.Expect(c.Status.ControlPlaneEndpoint.Host).To(Equal("api.e2e-test.example.com"))
			g.Expect(c.Status.Version).To(Equal("4.17.3"))
			g.Expect(c.Status.Phase).To(Equal(seaplanev1alpha1.ClusterPhaseReady))
		}).Should(Succeed())
	})

	It("should cascade delete NodePools, write delete desires, and remove Placement when Cluster is deleted", func() {
		By("creating a Cluster CR")
		cluster := newTestCluster(clusterName)
		Expect(createTestClusterWithReservation(cluster)).To(Succeed())

		By("waiting for PlacementRef to be set")
		Eventually(func(g Gomega) {
			var c seaplanev1alpha1.Cluster
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &c)).To(Succeed())
			g.Expect(c.Status.PlacementRef).NotTo(BeNil())
		}).Should(Succeed())
		var ownedPlacement seaplanev1alpha1.Placement
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName + ".placement"}, &ownedPlacement)).To(Succeed())
		Expect(ownedPlacement.Labels[seaplanev1alpha1.ClusterUIDLabel]).To(Equal(string(cluster.UID)))
		Expect(metav1.GetControllerOf(&ownedPlacement).UID).To(Equal(cluster.UID))

		By("creating a NodePool CR")
		np := newTestNodePool(cluster)
		Expect(k8sClient.Create(ctx, np)).To(Succeed())

		By("waiting for NodePool ApplyDesire to confirm both CRs are reconciled")
		specsApply := mc + "-specs-applydesires"
		Eventually(func(g Gomega) {
			items := scanTable(specsApply)
			for _, item := range items {
				if attrString(item, "spec", "targetItem", "resource") == "nodepools" {
					return
				}
			}
			g.Expect(false).To(BeTrue(), "nodepool desire not found yet")
		}).Should(Succeed())

		By("deleting the Cluster CR")
		var toDelete seaplanev1alpha1.Cluster
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &toDelete)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &toDelete)).To(Succeed())

		By("verifying NodePool CR is deleted")
		Eventually(func() error {
			nn := types.NamespacedName{Namespace: testNS, Name: clusterName + ".e2e-nodepool"}
			return k8sClient.Get(ctx, nn, &seaplanev1alpha1.NodePool{})
		}).ShouldNot(Succeed())

		By("verifying delete desire status entries exist in the applydesires status table")
		statusApply := mc + "-status-applydesires"
		Eventually(func(g Gomega) {
			// Status entries with Type=Delete prove the desires were created and confirmed.
			statusItems := scanTable(statusApply)
			g.Expect(len(statusItems)).To(BeNumerically(">=", 2), "expected status entries for processed delete desires")
		}).Should(Succeed())

		By("verifying ApplyDesire specs are cleaned up from DynamoDB")
		Eventually(func(g Gomega) {
			items := scanTable(specsApply)
			g.Expect(items).To(BeEmpty(), "all ApplyDesire specs should be cleaned up on deletion")
		}).Should(Succeed())

		By("verifying Placement CR is deleted")
		Eventually(func() error {
			nn := types.NamespacedName{
				Namespace: testNS,
				Name:      clusterName + ".placement",
			}
			return k8sClient.Get(ctx, nn, &seaplanev1alpha1.Placement{})
		}).ShouldNot(Succeed())

		By("verifying Cluster CR is fully gone")
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &seaplanev1alpha1.Cluster{})
		}).ShouldNot(Succeed())
	})

	It("should mark an expired cluster for deletion", func() {
		By("creating an expired Cluster CR")
		cluster := newExpiredTestCluster("e2e-expired-01")
		cluster.Name = clusterName
		Expect(createTestClusterWithReservation(cluster)).To(Succeed())

		By("verifying expiration sets a deletion timestamp")
		Eventually(func(g Gomega) {
			var current seaplanev1alpha1.Cluster
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &current)).To(Succeed())
			g.Expect(current.DeletionTimestamp.IsZero()).To(BeFalse())
		}).Should(Succeed())
	})

	It("should write NodePool ApplyDesire when NodePool CR is created", func() {
		By("creating a Cluster CR with PlacementRef")
		cluster := newTestCluster(clusterName)
		Expect(createTestClusterWithReservation(cluster)).To(Succeed())

		Eventually(func(g Gomega) {
			var c seaplanev1alpha1.Cluster
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &c)).To(Succeed())
			g.Expect(c.Status.PlacementRef).NotTo(BeNil())
		}).Should(Succeed())

		By("creating a NodePool CR")
		np := newTestNodePool(cluster)
		Expect(k8sClient.Create(ctx, np)).To(Succeed())

		By("waiting for NodePool ApplyDesire in DynamoDB")
		specsTable := mc + "-specs-applydesires"
		Eventually(func(g Gomega) {
			items := scanTable(specsTable)
			for _, item := range items {
				resource := attrString(item, "spec", "targetItem", "resource")
				if resource == "nodepools" {
					name := attrString(item, "spec", "targetItem", "name")
					g.Expect(name).To(Equal("e2e-nodepool"))
					return
				}
			}
			g.Expect(false).To(BeTrue(), "nodepool desire not found")
		}).Should(Succeed())
	})

	It("should write NodePool delete ApplyDesire when only the NodePool is deleted", func() {
		By("creating a Cluster CR")
		cluster := newTestCluster(clusterName)
		Expect(createTestClusterWithReservation(cluster)).To(Succeed())

		By("waiting for PlacementRef")
		Eventually(func(g Gomega) {
			var c seaplanev1alpha1.Cluster
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &c)).To(Succeed())
			g.Expect(c.Status.PlacementRef).NotTo(BeNil())
		}).Should(Succeed())

		By("creating a NodePool CR")
		np := newTestNodePool(cluster)
		Expect(k8sClient.Create(ctx, np)).To(Succeed())

		By("waiting for NodePool ApplyDesire in DynamoDB")
		specsApply := mc + "-specs-applydesires"
		Eventually(func(g Gomega) {
			items := scanTable(specsApply)
			for _, item := range items {
				if attrString(item, "spec", "targetItem", "resource") == "nodepools" {
					return
				}
			}
			g.Expect(false).To(BeTrue(), "nodepool desire not found")
		}).Should(Succeed())

		By("deleting only the NodePool CR")
		Eventually(func() error {
			var npToDelete seaplanev1alpha1.NodePool
			nn := types.NamespacedName{Namespace: testNS, Name: clusterName + ".e2e-nodepool"}
			if err := k8sClient.Get(ctx, nn, &npToDelete); err != nil {
				return err
			}
			return k8sClient.Delete(ctx, &npToDelete)
		}).Should(Succeed())

		By("verifying NodePool ApplyDesire is cleaned up from DynamoDB")
		Eventually(func(g Gomega) {
			items := scanTable(specsApply)
			for _, item := range items {
				g.Expect(attrString(item, "spec", "targetItem", "resource")).NotTo(Equal("nodepools"),
					"nodepool ApplyDesire should be cleaned up on deletion")
			}
		}).Should(Succeed())

		By("verifying NodePool delete ApplyDesire was processed and status recorded in DynamoDB")
		statusApply := mc + "-status-applydesires"
		Eventually(func(g Gomega) {
			// Status entry proves the delete desire was created and confirmed.
			statusItems := scanTable(statusApply)
			found := false
			for _, item := range statusItems {
				if docID, ok := item["documentID"]; ok {
					if sv, ok := docID.(*dynamodbtypes.AttributeValueMemberS); ok && sv.Value != "" {
						found = true
						break
					}
				}
			}
			g.Expect(found).To(BeTrue(), "expected status entry for processed nodepool delete ApplyDesire")
		}).Should(Succeed())

		By("verifying NodePool CR is fully gone")
		Eventually(func() error {
			nn := types.NamespacedName{Namespace: testNS, Name: clusterName + ".e2e-nodepool"}
			return k8sClient.Get(ctx, nn, &seaplanev1alpha1.NodePool{})
		}).ShouldNot(Succeed())

		By("verifying Cluster and Placement are still alive")
		var c seaplanev1alpha1.Cluster
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &c)).To(Succeed())
		var p seaplanev1alpha1.Placement
		nn := types.NamespacedName{
			Namespace: testNS,
			Name:      clusterName + ".placement",
		}
		Expect(k8sClient.Get(ctx, nn, &p)).To(Succeed())
	})
})
