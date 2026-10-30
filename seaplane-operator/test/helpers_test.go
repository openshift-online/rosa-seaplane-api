package integration

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/api/util/ipnet"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newExpiredTestCluster(name string) *seaplanev1alpha1.Cluster {
	cluster := newTestCluster(name)
	expiry := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	cluster.Spec.ExpirationTimestamp = &expiry
	return cluster
}

var _ = BeforeEach(func() {
	purgeResources()
	purgeDynamoTables()
	dynamoCli.ResetCache()
})

func purgeResources() {
	clearFinalizersAndDelete()

	// Poll until all resources are gone, re-clearing finalizers on each pass.
	// The controller may re-add a finalizer between our Update and Delete, leaving
	// the object stuck in terminating. Returning an error from Update lets
	// Eventually retry with a fresh List that has the current ResourceVersion.
	Eventually(waitForResourcesGone, 15*time.Second, 100*time.Millisecond).Should(Equal(0))
}

// clearFinalizersAndDelete issues best-effort delete requests for all test
// resources with their finalizers cleared.
func clearFinalizersAndDelete() {
	c := mgr.GetClient()

	// Issue the initial delete requests with finalizers cleared.
	var clusters seaplanev1alpha1.ClusterList
	if err := c.List(ctx, &clusters); err == nil {
		for i := range clusters.Items {
			clusters.Items[i].SetFinalizers(nil)
			_ = c.Update(ctx, &clusters.Items[i])
			_ = c.Delete(ctx, &clusters.Items[i])
		}
	}
	var nodepools seaplanev1alpha1.NodePoolList
	if err := c.List(ctx, &nodepools); err == nil {
		for i := range nodepools.Items {
			nodepools.Items[i].SetFinalizers(nil)
			_ = c.Update(ctx, &nodepools.Items[i])
			_ = c.Delete(ctx, &nodepools.Items[i])
		}
	}
	var manifests seaplanev1alpha1.ManifestList
	if err := c.List(ctx, &manifests); err == nil {
		for i := range manifests.Items {
			manifests.Items[i].SetFinalizers(nil)
			_ = c.Update(ctx, &manifests.Items[i])
			_ = c.Delete(ctx, &manifests.Items[i])
		}
	}
	var oidcConfigs seaplanev1alpha1.OidcConfigList
	if err := c.List(ctx, &oidcConfigs); err == nil {
		for i := range oidcConfigs.Items {
			oidcConfigs.Items[i].SetFinalizers(nil)
			_ = c.Update(ctx, &oidcConfigs.Items[i])
			_ = c.Delete(ctx, &oidcConfigs.Items[i])
		}
	}
	var placements seaplanev1alpha1.PlacementList
	if err := c.List(ctx, &placements); err == nil {
		for i := range placements.Items {
			_ = c.Delete(ctx, &placements.Items[i])
		}
	}
	var dnsReservations seaplanev1alpha1.DNSReservationList
	if err := c.List(ctx, &dnsReservations); err == nil {
		for i := range dnsReservations.Items {
			dnsReservations.Items[i].SetFinalizers(nil)
			_ = c.Update(ctx, &dnsReservations.Items[i])
			_ = c.Delete(ctx, &dnsReservations.Items[i])
		}
	}
	var indexes seaplanev1alpha1.IndexList
	if err := c.List(ctx, &indexes); err == nil {
		for i := range indexes.Items {
			_ = c.Delete(ctx, &indexes.Items[i])
		}
	}
}

// waitForResourcesGone lists the finalizer-bearing test resources, re-clears any
// finalizers a controller may have re-added, deletes leftovers, and reports how
// many remain so an Eventually caller can retry until zero.
func waitForResourcesGone() (int, error) {
	c := mgr.GetClient()

	// Bind each retry attempt with a deadline to prevent indefinite blocking.
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	total := 0
	var cl seaplanev1alpha1.ClusterList
	if err := c.List(opCtx, &cl); err != nil {
		return 0, err
	}
	for i := range cl.Items {
		if len(cl.Items[i].Finalizers) > 0 {
			cl.Items[i].SetFinalizers(nil)
			if err := c.Update(opCtx, &cl.Items[i]); err != nil {
				return 0, err
			}
		}
		// Delete after clearing finalizers; treat NotFound as success (already deleted).
		if err := c.Delete(opCtx, &cl.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	total += len(cl.Items)

	var nl seaplanev1alpha1.NodePoolList
	if err := c.List(opCtx, &nl); err != nil {
		return 0, err
	}
	for i := range nl.Items {
		if len(nl.Items[i].Finalizers) > 0 {
			nl.Items[i].SetFinalizers(nil)
			if err := c.Update(opCtx, &nl.Items[i]); err != nil {
				return 0, err
			}
		}
		// Delete after clearing finalizers; treat NotFound as success (already deleted).
		if err := c.Delete(opCtx, &nl.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	total += len(nl.Items)

	var ml seaplanev1alpha1.ManifestList
	if err := c.List(opCtx, &ml); err != nil {
		return 0, err
	}
	for i := range ml.Items {
		if len(ml.Items[i].Finalizers) > 0 {
			ml.Items[i].SetFinalizers(nil)
			if err := c.Update(opCtx, &ml.Items[i]); err != nil {
				return 0, err
			}
		}
		// Delete after clearing finalizers; treat NotFound as success (already deleted).
		if err := c.Delete(opCtx, &ml.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	total += len(ml.Items)

	for _, cleanup := range []func(context.Context, client.Client) (int, error){
		cleanupTestPlacements,
		cleanupTestDNSReservations,
		cleanupTestIndexes,
	} {
		count, err := cleanup(opCtx, c)
		if err != nil {
			return 0, err
		}
		total += count
	}

	return total, nil
}

func cleanupTestPlacements(ctx context.Context, c client.Client) (int, error) {
	var placements seaplanev1alpha1.PlacementList
	if err := c.List(ctx, &placements); err != nil {
		return 0, err
	}
	for i := range placements.Items {
		if err := c.Delete(ctx, &placements.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	return len(placements.Items), nil
}

func cleanupTestDNSReservations(ctx context.Context, c client.Client) (int, error) {
	var reservations seaplanev1alpha1.DNSReservationList
	if err := c.List(ctx, &reservations); err != nil {
		return 0, err
	}
	for i := range reservations.Items {
		reservations.Items[i].SetFinalizers(nil)
		if err := c.Update(ctx, &reservations.Items[i]); err != nil {
			return 0, err
		}
		if err := c.Delete(ctx, &reservations.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	return len(reservations.Items), nil
}

func cleanupTestIndexes(ctx context.Context, c client.Client) (int, error) {
	var indexes seaplanev1alpha1.IndexList
	if err := c.List(ctx, &indexes); err != nil {
		return 0, err
	}
	for i := range indexes.Items {
		if err := c.Delete(ctx, &indexes.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	return len(indexes.Items), nil
}

func scanTable(tableName string) []map[string]dynamodbtypes.AttributeValue {
	out, err := dynamoDBCli.Scan(ctx, &dynamodb.ScanInput{
		TableName: aws.String(tableName),
	})
	Expect(err).NotTo(HaveOccurred())
	return out.Items
}

func attrString(item map[string]dynamodbtypes.AttributeValue, keys ...string) string {
	current := item
	for i, key := range keys {
		av, ok := current[key]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			if sv, ok := av.(*dynamodbtypes.AttributeValueMemberS); ok {
				return sv.Value
			}
			return ""
		}
		if mv, ok := av.(*dynamodbtypes.AttributeValueMemberM); ok {
			current = mv.Value
		} else {
			return ""
		}
	}
	return ""
}

func purgeTable(tableName string) {
	items := scanTable(tableName)
	for _, item := range items {
		if docID, ok := item["documentID"]; ok {
			_, _ = dynamoDBCli.DeleteItem(ctx, &dynamodb.DeleteItemInput{
				TableName: aws.String(tableName),
				Key:       map[string]dynamodbtypes.AttributeValue{"documentID": docID},
			})
		}
	}
}

func purgeDynamoTables() {
	suffixes := []string{"-applydesires", "-readdesires"}
	for _, prefix := range []string{mc + "-specs", mc + "-status"} {
		for _, suffix := range suffixes {
			purgeTable(prefix + suffix)
		}
	}
}

func mustParseCIDR(s string) ipnet.IPNet {
	parsed, err := ipnet.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return *parsed
}

func newTestCluster(name string) *seaplanev1alpha1.Cluster {
	return &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "account-111222333444",
			Labels: map[string]string{
				seaplanev1alpha1.AccountIDLabel: "111222333444",
			},
		},
		Spec: seaplanev1alpha1.ClusterSpec{
			AccountID:         "111222333444",
			CreatorARN:        "arn:aws:iam::111222333444:user/e2etester",
			HostedClusterName: seaplanev1alpha1.HostedClusterNamePending,
			HostedCluster: seaplanev1alpha1.HostedClusterSpecPassthrough{
				Release:    hypershiftv1beta1.Release{Image: "quay.io/ocp:4.17"},
				IssuerURL:  "https://oidc.e2e.example.com/" + name,
				PullSecret: corev1.LocalObjectReference{Name: "pull-secret"},
				Networking: hypershiftv1beta1.ClusterNetworking{
					ClusterNetwork: []hypershiftv1beta1.ClusterNetworkEntry{{CIDR: mustParseCIDR("10.128.0.0/14")}},
					ServiceNetwork: []hypershiftv1beta1.ServiceNetworkEntry{{CIDR: mustParseCIDR("172.30.0.0/16")}},
					MachineNetwork: []hypershiftv1beta1.MachineNetworkEntry{{CIDR: mustParseCIDR("10.0.0.0/16")}},
				},
				Etcd: hypershiftv1beta1.EtcdSpec{
					ManagementType: hypershiftv1beta1.Managed,
					Managed: &hypershiftv1beta1.ManagedEtcdSpec{
						Storage: hypershiftv1beta1.ManagedEtcdStorageSpec{
							Type: hypershiftv1beta1.PersistentVolumeEtcdStorage,
						},
					},
				},
				Services: []hypershiftv1beta1.ServicePublishingStrategyMapping{
					{
						Service:                   hypershiftv1beta1.APIServer,
						ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{Type: hypershiftv1beta1.Route},
					},
					{
						Service:                   hypershiftv1beta1.OAuthServer,
						ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{Type: hypershiftv1beta1.Route},
					},
					{
						Service:                   hypershiftv1beta1.Konnectivity,
						ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{Type: hypershiftv1beta1.Route},
					},
					{
						Service:                   hypershiftv1beta1.Ignition,
						ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{Type: hypershiftv1beta1.Route},
					},
				},
				Platform: hypershiftv1beta1.PlatformSpec{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS: &hypershiftv1beta1.AWSPlatformSpec{
						Region: "us-east-1",
						CloudProviderConfig: &hypershiftv1beta1.AWSCloudProviderConfig{
							VPC:  "vpc-e2e0001",
							Zone: "us-east-1a",
							Subnet: &hypershiftv1beta1.AWSResourceReference{
								ID: ptr.To("subnet-e2e0001"),
							},
						},
						RolesRef: hypershiftv1beta1.AWSRolesRef{
							ControlPlaneOperatorARN: "arn:aws:iam::111222333444:role/cpo",
							IngressARN:              "arn:aws:iam::111222333444:role/ingress",
							ImageRegistryARN:        "arn:aws:iam::111222333444:role/registry",
							KubeCloudControllerARN:  "arn:aws:iam::111222333444:role/kccm",
							NodePoolManagementARN:   "arn:aws:iam::111222333444:role/npm",
							NetworkARN:              "arn:aws:iam::111222333444:role/network",
							StorageARN:              "arn:aws:iam::111222333444:role/storage",
						},
					},
				},
			},
		},
	}
}

// createTestClusterWithReservation simulates the customer-first DNS workflow:
// create and await a reservation, copy its published internal HostedCluster
// name into the Cluster, then bind the reservation to the new Cluster UID.
func createTestClusterWithReservation(cluster *seaplanev1alpha1.Cluster) error {
	accountID := cluster.Spec.AccountID
	if accountID == "" {
		return fmt.Errorf("test Cluster %s has no account ID", cluster.Name)
	}
	cluster.Namespace = "account-" + accountID
	if cluster.Labels == nil {
		cluster.Labels = make(map[string]string)
	}
	cluster.Labels[seaplanev1alpha1.AccountIDLabel] = accountID

	reservation := &seaplanev1alpha1.DNSReservation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dns-" + cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
		},
	}
	if err := k8sClient.Create(ctx, reservation); err != nil {
		return err
	}
	Eventually(func(g Gomega) {
		var latest seaplanev1alpha1.DNSReservation
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(reservation), &latest)).To(Succeed())
		g.Expect(latest.Status.Phase).To(Equal(seaplanev1alpha1.DNSReservationPhaseReady))
		g.Expect(latest.Status.BaseDomain).NotTo(BeEmpty())
		g.Expect(latest.Status.InternalHostedClusterName).NotTo(BeEmpty())
		*reservation = latest
	}).Should(Succeed())

	cluster.Spec.DNSReservationID = string(reservation.UID)
	cluster.Spec.HostedClusterName = reservation.Status.InternalHostedClusterName
	if err := k8sClient.Create(ctx, cluster); err != nil {
		return err
	}
	reservation.Labels[seaplanev1alpha1.ClaimedByClusterUIDLabel] = string(cluster.UID)
	if err := k8sClient.Update(ctx, reservation); err != nil {
		return err
	}

	if cluster.Spec.OidcConfigID != "" {
		var configs seaplanev1alpha1.OidcConfigList
		if err := k8sClient.List(ctx, &configs, client.InNamespace(cluster.Namespace)); err != nil {
			return err
		}
		for i := range configs.Items {
			config := &configs.Items[i]
			if string(config.UID) != cluster.Spec.OidcConfigID {
				continue
			}
			if config.Labels == nil {
				config.Labels = make(map[string]string)
			}
			config.Labels[seaplanev1alpha1.ClaimedByClusterUIDLabel] = string(cluster.UID)
			return k8sClient.Update(ctx, config)
		}
		return fmt.Errorf("test OidcConfig UID %s not found in %s", cluster.Spec.OidcConfigID, cluster.Namespace)
	}
	return nil
}

// newTestClusterWithOidcConfig returns a cluster fixture using the
// OidcConfig-backed issuer path (OidcConfigID set).
func newTestClusterWithOidcConfig(name string) *seaplanev1alpha1.Cluster {
	return newTestCluster(name)
}

// newTestOidcConfig returns an unmanaged OidcConfig fixture matching the
// OidcConfigID/account label set by newTestClusterWithOidcConfig, so the
// operator's ClusterReconciler resolves oidcSigningKeyExternal=true for it.
func newTestOidcConfig() *seaplanev1alpha1.OidcConfig {
	return &seaplanev1alpha1.OidcConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-oidc-config", Namespace: "account-111222333444"},
		Spec: seaplanev1alpha1.OidcConfigSpec{
			Type:             seaplanev1alpha1.OidcConfigTypeUnmanaged,
			IssuerUrl:        "https://oidc.e2e.example.com/e2e-oidc-config",
			SecretArn:        "arn:aws:secretsmanager:us-east-1:111222333444:secret:e2e-test",
			InstallerRoleArn: "arn:aws:iam::111222333444:role/installer",
			AccountID:        "111222333444",
		},
	}
}

func newTestNodePool(cluster *seaplanev1alpha1.Cluster) *seaplanev1alpha1.NodePool {
	nodePool := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name + ".e2e-nodepool", Namespace: cluster.Namespace},
		Spec: seaplanev1alpha1.NodePoolSpec{
			NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
				ClusterName: cluster.Name,
				Replicas:    ptr.To(int32(3)),
				Management: hypershiftv1beta1.NodePoolManagement{
					AutoRepair:  true,
					UpgradeType: hypershiftv1beta1.UpgradeTypeReplace,
				},
				Release: hypershiftv1beta1.Release{Image: "quay.io/ocp:4.17"},
				Platform: hypershiftv1beta1.NodePoolPlatform{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS: &hypershiftv1beta1.AWSNodePoolPlatform{
						InstanceType:    "m6a.xlarge",
						RootVolume:      &hypershiftv1beta1.Volume{Size: 120, Type: "gp3"},
						InstanceProfile: "worker-profile",
						Subnet: hypershiftv1beta1.AWSResourceReference{
							ID: ptr.To("subnet-e2e0001"),
						},
						SecurityGroups: []hypershiftv1beta1.AWSResourceReference{
							{ID: ptr.To("sg-e2e0001")},
						},
					},
				},
			},
		},
	}
	nodePool.Labels = map[string]string{
		seaplanev1alpha1.ClusterUIDLabel: string(cluster.UID),
	}
	nodePool.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(cluster, seaplanev1alpha1.GroupVersion.WithKind("Cluster"))}
	return nodePool
}

func newTestManifest(name string) *seaplanev1alpha1.Manifest {
	return &seaplanev1alpha1.Manifest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "mc01",
		},
		Spec: seaplanev1alpha1.ManifestSpec{
			ManagementCluster: "mc01",
			Resources: []seaplanev1alpha1.ResourceTemplate{
				{
					Resource: "serviceaccounts",
					Content: runtime.RawExtension{Raw: []byte(
						`{"apiVersion":"v1","kind":"ServiceAccount",` +
							`"metadata":{"name":"e2e-runner","namespace":"e2e-actions"}}`,
					)},
				},
				{
					Resource: "roles",
					Content: runtime.RawExtension{Raw: []byte(
						`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role",` +
							`"metadata":{"name":"e2e-runner","namespace":"e2e-actions"},` +
							`"rules":[{"apiGroups":[""],"resources":["pods/log"],"verbs":["get"]}]}`,
					)},
				},
				{
					Resource: "rolebindings",
					Content: runtime.RawExtension{Raw: []byte(
						`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding",` +
							`"metadata":{"name":"e2e-runner","namespace":"e2e-actions"},` +
							`"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":"e2e-runner"},` +
							`"subjects":[{"kind":"ServiceAccount","name":"e2e-runner","namespace":"e2e-actions"}]}`,
					)},
				},
				{
					Resource: "jobs",
					Content: runtime.RawExtension{Raw: []byte(
						`{"apiVersion":"batch/v1","kind":"Job",` +
							`"metadata":{"name":"e2e-job-abc123","namespace":"e2e-actions"},` +
							`"spec":{"template":{"spec":{"serviceAccountName":"e2e-runner",` +
							`"containers":[{"name":"runner","image":"registry.example.com/e2e-runner:latest"}],` +
							`"restartPolicy":"Never"}}}}`,
					)},
					Watch: true,
				},
			},
		},
	}
}
