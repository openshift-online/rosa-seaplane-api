/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/render"
)

var _ = Describe("Placement Controller", func() {
	const (
		clusterName  = "test-placement-cluster"
		clusterName2 = "test-placement-cluster-2"
		testNS       = "account-123456789012"
		mcName       = "mc01"
		mc2Name      = "mc02"
	)
	ctx := context.Background()

	newReconciler := func(c client.Client, slots int) *PlacementReconciler {
		return &PlacementReconciler{Client: c, Scheme: k8sClient.Scheme(), HCPSlotCount: slots}
	}
	reconcileCluster := func(r *PlacementReconciler, name string) (ctrl.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: name}})
	}
	createCluster := func(name string) *seaplanev1alpha1.Cluster {
		cluster := newTestCluster(name)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		return cluster
	}
	createMC := func(name string) {
		ensureNamespace(ctx, hcpSlotNamespace(name))
		mc := &seaplanev1alpha1.ManagementCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       seaplanev1alpha1.ManagementClusterSpec{Region: "us-east-1", AccountID: "123456789012"},
		}
		if err := k8sClient.Create(ctx, mc); err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}
	}
	createClaim := func(mc string, slot int32, ownerUID types.UID) {
		claim := &seaplanev1alpha1.Index{ObjectMeta: metav1.ObjectMeta{
			Namespace: hcpSlotNamespace(mc),
			Name:      render.HCPSlotName(slot),
			Labels:    map[string]string{ownerUIDLabel: string(ownerUID)},
		}}
		Expect(k8sClient.Create(ctx, claim)).To(Succeed())
	}
	getClaim := func(mc string, slot int32) (*seaplanev1alpha1.Index, error) {
		claim := &seaplanev1alpha1.Index{}
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: hcpSlotNamespace(mc), Name: render.HCPSlotName(slot)}, claim)
		return claim, err
	}
	getPlacement := func(name string) (*seaplanev1alpha1.Placement, error) {
		placement := &seaplanev1alpha1.Placement{}
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: name + ".placement"}, placement)
		return placement, err
	}
	expectGone := func(obj client.Object, key types.NamespacedName) {
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, obj))).To(BeTrue(), "expected %s to be gone", key)
	}

	BeforeEach(func() {
		ensureNamespace(ctx, testNS)
		createMC(mcName)
	})

	AfterEach(func() {
		for _, name := range []string{clusterName, clusterName2} {
			if placement, err := getPlacement(name); err == nil {
				controllerutil.RemoveFinalizer(placement, hcpSlotFinalizer)
				Expect(client.IgnoreNotFound(k8sClient.Update(ctx, placement))).To(Succeed())
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, placement))).To(Succeed())
				expectGone(&seaplanev1alpha1.Placement{}, client.ObjectKeyFromObject(placement))
			}
			cluster := &seaplanev1alpha1.Cluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: name}, cluster); err == nil {
				controllerutil.RemoveFinalizer(cluster, clusterFinalizer)
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}
		}
		for _, mc := range []string{mcName, mc2Name} {
			var claims seaplanev1alpha1.IndexList
			Expect(k8sClient.List(ctx, &claims, client.InNamespace(hcpSlotNamespace(mc)))).To(Succeed())
			for i := range claims.Items {
				_ = k8sClient.Delete(ctx, &claims.Items[i])
			}
		}
		_ = k8sClient.Delete(ctx, &seaplanev1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: mc2Name}})
	})

	It("creates a Bound Placement on the lowest free HCP slot", func() {
		cluster := createCluster(clusterName)

		_, err := reconcileCluster(newReconciler(k8sClient, 3), clusterName)
		Expect(err).NotTo(HaveOccurred())

		placement, err := getPlacement(clusterName)
		Expect(err).NotTo(HaveOccurred())
		Expect(placement.Spec.ClusterName).To(Equal(clusterName))
		Expect(placement.Spec.ManagementCluster).To(Equal(mcName))
		Expect(placement.Spec.HCPSlot).To(Equal(int32(1)))
		Expect(placement.Labels[seaplanev1alpha1.ClusterUIDLabel]).To(Equal(string(cluster.UID)))
		Expect(placement.Finalizers).To(ContainElement(hcpSlotFinalizer))
		Expect(placement.Status.Phase).To(Equal(seaplanev1alpha1.PlacementPhaseBound))

		claim, err := getClaim(mcName, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.Labels[ownerUIDLabel]).To(Equal(string(cluster.UID)))
	})

	It("updates Cluster status with placementRef", func() {
		createCluster(clusterName)

		_, err := reconcileCluster(newReconciler(k8sClient, 3), clusterName)
		Expect(err).NotTo(HaveOccurred())

		var cluster seaplanev1alpha1.Cluster
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &cluster)).To(Succeed())
		Expect(cluster.Status.PlacementRef).NotTo(BeNil())
		Expect(cluster.Status.PlacementRef.ManagementCluster).To(Equal(mcName))
	})

	It("places each Cluster on its own slot", func() {
		createCluster(clusterName)
		createCluster(clusterName2)
		r := newReconciler(k8sClient, 3)

		for _, name := range []string{clusterName, clusterName2} {
			_, err := reconcileCluster(r, name)
			Expect(err).NotTo(HaveOccurred())
		}

		first, err := getPlacement(clusterName)
		Expect(err).NotTo(HaveOccurred())
		second, err := getPlacement(clusterName2)
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Spec.HCPSlot).To(Equal(int32(1)))
		Expect(second.Spec.HCPSlot).To(Equal(int32(2)))
	})

	It("skips a management cluster with no free slot", func() {
		createMC(mc2Name)
		createClaim(mcName, 1, "another-cluster")
		createCluster(clusterName)

		_, err := reconcileCluster(newReconciler(k8sClient, 1), clusterName)
		Expect(err).NotTo(HaveOccurred())

		placement, err := getPlacement(clusterName)
		Expect(err).NotTo(HaveOccurred())
		Expect(placement.Spec.ManagementCluster).To(Equal(mc2Name))
		Expect(placement.Spec.HCPSlot).To(Equal(int32(1)))
	})

	It("requeues without placing when every slot is taken", func() {
		createClaim(mcName, 1, "another-cluster")
		createCluster(clusterName)

		result, err := reconcileCluster(newReconciler(k8sClient, 1), clusterName)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		_, err = getPlacement(clusterName)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("reuses a slot the Cluster already claimed", func() {
		cluster := createCluster(clusterName)
		createClaim(mcName, 2, cluster.UID)

		_, err := reconcileCluster(newReconciler(k8sClient, 3), clusterName)
		Expect(err).NotTo(HaveOccurred())

		placement, err := getPlacement(clusterName)
		Expect(err).NotTo(HaveOccurred())
		Expect(placement.Spec.HCPSlot).To(Equal(int32(2)))
		_, err = getClaim(mcName, 1)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("reuses a slot the Cluster already claimed on another management cluster", func() {
		createMC(mc2Name)
		cluster := createCluster(clusterName)
		createClaim(mc2Name, 2, cluster.UID)

		_, err := reconcileCluster(newReconciler(k8sClient, 3), clusterName)
		Expect(err).NotTo(HaveOccurred())

		placement, err := getPlacement(clusterName)
		Expect(err).NotTo(HaveOccurred())
		Expect(placement.Spec.ManagementCluster).To(Equal(mc2Name))
		Expect(placement.Spec.HCPSlot).To(Equal(int32(2)))
		_, err = getClaim(mcName, 1)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no second slot may be claimed on mc01")
	})

	It("gives back its slot when a racing attempt wrote the Placement first", func() {
		cluster := createCluster(clusterName)
		// The racing attempt claims slot 3 and writes the Placement just before ours.
		hook := &createHook{Client: k8sClient, before: func(obj client.Object) {
			if _, ok := obj.(*seaplanev1alpha1.Placement); !ok {
				return
			}
			createClaim(mcName, 3, cluster.UID)
			winner := obj.DeepCopyObject().(*seaplanev1alpha1.Placement)
			winner.Spec.HCPSlot = 3
			Expect(k8sClient.Create(ctx, winner)).To(Succeed())
		}}

		_, err := reconcileCluster(newReconciler(hook, 3), clusterName)
		Expect(err).NotTo(HaveOccurred())

		placement, err := getPlacement(clusterName)
		Expect(err).NotTo(HaveOccurred())
		Expect(placement.Spec.HCPSlot).To(Equal(int32(3)))
		Expect(placement.Status.Phase).To(Equal(seaplanev1alpha1.PlacementPhaseBound))
		_, err = getClaim(mcName, 1)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "our claim on slot 1 must be released")
		_, err = getClaim(mcName, 3)
		Expect(err).NotTo(HaveOccurred())
	})

	It("releases the slot when the Placement is deleted", func() {
		createCluster(clusterName)
		r := newReconciler(k8sClient, 3)
		_, err := reconcileCluster(r, clusterName)
		Expect(err).NotTo(HaveOccurred())
		placement, err := getPlacement(clusterName)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Delete(ctx, placement)).To(Succeed())
		_, err = getClaim(mcName, 1)
		Expect(err).NotTo(HaveOccurred(), "the finalizer keeps the slot until it runs")

		_, err = reconcileCluster(r, clusterName)
		Expect(err).NotTo(HaveOccurred())
		expectGone(&seaplanev1alpha1.Placement{}, client.ObjectKeyFromObject(placement))
		_, err = getClaim(mcName, 1)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("never releases a slot another Cluster claimed since", func() {
		createCluster(clusterName)
		r := newReconciler(k8sClient, 3)
		_, err := reconcileCluster(r, clusterName)
		Expect(err).NotTo(HaveOccurred())
		placement, err := getPlacement(clusterName)
		Expect(err).NotTo(HaveOccurred())

		// An earlier finalizer run released slot 1 and another Cluster claimed it.
		claim, err := getClaim(mcName, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Delete(ctx, claim)).To(Succeed())
		createClaim(mcName, 1, "another-cluster")

		Expect(k8sClient.Delete(ctx, placement)).To(Succeed())
		_, err = reconcileCluster(r, clusterName)
		Expect(err).NotTo(HaveOccurred())

		expectGone(&seaplanev1alpha1.Placement{}, client.ObjectKeyFromObject(placement))
		claim, err = getClaim(mcName, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.Labels[ownerUIDLabel]).To(Equal("another-cluster"))
	})
})

// createHook runs before on every Create, to simulate a write landing first.
type createHook struct {
	client.Client
	before func(obj client.Object)
}

func (h *createHook) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if h.before != nil {
		h.before(obj)
	}
	return h.Client.Create(ctx, obj, opts...)
}
