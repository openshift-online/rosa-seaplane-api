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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
)

var _ = Describe("Placement Controller", func() {
	Context("When reconciling a Cluster", func() {
		const (
			clusterName = "test-placement-cluster"
			testNS      = "account-123456789012"
		)

		ctx := context.Background()

		const mcName = "mc01"

		BeforeEach(func() {
			ensureNamespace(ctx, testNS)
			mc := &seaplanev1alpha1.ManagementCluster{
				ObjectMeta: metav1.ObjectMeta{Name: mcName},
				Spec: seaplanev1alpha1.ManagementClusterSpec{
					Region:    "us-east-1",
					AccountID: "123456789012",
				},
			}
			err := k8sClient.Create(ctx, mc)
			if err != nil {
				Expect(err.Error()).To(ContainSubstring("already exists"))
			}
		})

		AfterEach(func() {
			cluster := &seaplanev1alpha1.Cluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, cluster); err == nil {
				controllerutil.RemoveFinalizer(cluster, clusterFinalizer)
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}
			placement := &seaplanev1alpha1.Placement{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName + ".placement"}, placement); err == nil {
				_ = k8sClient.Delete(ctx, placement)
			}
		})

		It("should create a Placement for a new Cluster", func() {
			resource := newTestCluster(clusterName)
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			reconciler := &PlacementReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())

			var placement seaplanev1alpha1.Placement
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName + ".placement"}, &placement)).To(Succeed())
			Expect(placement.Spec.ClusterName).To(Equal(clusterName))
			Expect(placement.Spec.ManagementCluster).To(Equal(mcName))
			Expect(placement.Labels[seaplanev1alpha1.ClusterUIDLabel]).To(Equal(string(resource.UID)))
			Expect(placement.Status.Phase).To(Equal(seaplanev1alpha1.PlacementPhaseBound))
		})

		It("should update Cluster status with placementRef", func() {
			resource := newTestCluster(clusterName)
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			reconciler := &PlacementReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())

			var cluster seaplanev1alpha1.Cluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &cluster)).To(Succeed())
			Expect(cluster.Status.PlacementRef).NotTo(BeNil())
			Expect(cluster.Status.PlacementRef.ManagementCluster).To(Equal(mcName))
		})
	})
})
