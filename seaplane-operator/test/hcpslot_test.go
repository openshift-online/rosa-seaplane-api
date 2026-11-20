package integration

import (
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/controller"
)

var _ = Describe("HCP slot placement", func() {
	const (
		mc2         = "mc02"
		clusters    = 8
		reconcilers = 3
	)

	AfterEach(func() {
		purgeResources()
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &seaplanev1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: mc2}}))).To(Succeed())
	})

	It("never gives two Clusters the same slot and releases every claim", func() {
		By("adding a second management cluster")
		Expect(k8sClient.Create(ctx, &seaplanev1alpha1.ManagementCluster{
			ObjectMeta: metav1.ObjectMeta{Name: mc2},
			Spec:       seaplanev1alpha1.ManagementClusterSpec{Region: "us-east-1", AccountID: "111222333444"},
		})).To(Succeed())

		By("creating more Clusters than there are slots")
		var names []string
		var namespace string
		for i := range clusters {
			cluster := newTestCluster(fmt.Sprintf("slot-race-%02d", i))
			namespace = "account-" + cluster.Spec.AccountID
			cluster.Namespace = namespace
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			names = append(names, cluster.Name)
		}

		By("placing them from several reconcilers at once, racing the running operator")
		var wg sync.WaitGroup
		for range reconcilers {
			r := &controller.PlacementReconciler{Client: k8sClient, Scheme: mgr.GetScheme(), HCPSlotCount: hcpSlotCount}
			for _, name := range names {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}})
					Expect(err).NotTo(HaveOccurred())
				}()
			}
		}
		wg.Wait()

		By("checking every (management cluster, slot) is held by exactly one Placement and one claim")
		// A Cluster that saw every MC full while a racing attempt held an extra claim retries after its requeue.
		var placements seaplanev1alpha1.PlacementList
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.List(ctx, &placements, client.InNamespace(namespace))).To(Succeed())
			bySlot := map[string]string{}
			for _, p := range placements.Items {
				key := fmt.Sprintf("%s/%02d", p.Spec.ManagementCluster, p.Spec.HCPSlot)
				Expect(bySlot).NotTo(HaveKey(key), "slot %s given to two Clusters", key)
				bySlot[key] = p.Labels[seaplanev1alpha1.ClusterUIDLabel]
			}
			g.Expect(placements.Items).To(HaveLen(2 * hcpSlotCount))
			g.Expect(slotClaims(mc, mc2)).To(Equal(bySlot), "claims must match Placements exactly: no leaked or missing claim")
		}, 45*time.Second, time.Second).Should(Succeed())

		By("deleting the Clusters and their Placements releases every slot")
		// Clusters first, so a freed slot isn't handed to one of them again.
		for _, name := range names {
			var cluster seaplanev1alpha1.Cluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &cluster)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &cluster)).To(Succeed())
		}
		for i := range placements.Items {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &placements.Items[i]))).To(Succeed())
		}
		Eventually(func() map[string]string { return slotClaims(mc, mc2) }).Should(BeEmpty())
	})
})

// slotClaims maps "mc/NN" to the holder UID of every HCP slot claim on the given MCs.
func slotClaims(mcs ...string) map[string]string {
	claims := map[string]string{}
	for _, m := range mcs {
		var list seaplanev1alpha1.IndexList
		Expect(k8sClient.List(ctx, &list, client.InNamespace("hcp-slots-"+m))).To(Succeed())
		for _, c := range list.Items {
			claims[m+"/"+c.Name] = c.Labels["seaplane.io/owner-uid"]
		}
	}
	return claims
}
