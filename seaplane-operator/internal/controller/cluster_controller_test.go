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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/dynamo"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/render"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/api/util/ipnet"
)

var _ = Describe("Cluster Controller", func() {
	Context("When reconciling a new Cluster", func() {
		const (
			clusterName = "test-cluster-01"
			testNS      = "account-123456789012"
		)

		ctx := context.Background()

		BeforeEach(func() {
			ensureNamespace(ctx, testNS)
			// Customer resources and the reservation share the account namespace;
			// DNS Indexes use the operator-controlled shard namespace.
			ensureNamespace(ctx, "dns-shard-0-reservations")
		})

		AfterEach(func() {
			resource := &seaplanev1alpha1.Cluster{}
			var clusterUID string
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, resource)
			if err == nil {
				clusterUID = string(resource.UID)
				controllerutil.RemoveFinalizer(resource, clusterFinalizer)
				_ = k8sClient.Update(ctx, resource)
				_ = k8sClient.Delete(ctx, resource)
			}
			placement := &seaplanev1alpha1.Placement{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName + ".placement"}, placement); err == nil {
				_ = k8sClient.Delete(ctx, placement)
			}
			oc := &seaplanev1alpha1.OidcConfig{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: "test-oidc-config"}, oc); err == nil {
				_ = k8sClient.Delete(ctx, oc)
			}
			var reservations seaplanev1alpha1.DNSReservationList
			if err := k8sClient.List(ctx, &reservations, client.InNamespace(testNS)); err == nil {
				for i := range reservations.Items {
					reservation := &reservations.Items[i]
					if reservation.Name != "dns-"+clusterName &&
						(clusterUID == "" || reservation.Labels[claimedByClusterUIDLabel] != clusterUID) {
						continue
					}
					controllerutil.RemoveFinalizer(reservation, dnsReservationFinalizer)
					_ = k8sClient.Update(ctx, reservation)
					_ = k8sClient.Delete(ctx, reservation)
				}
			}
		})

		It("should add a finalizer on first reconcile", func() {
			resource := newTestCluster(clusterName)
			Expect(createTestClusterWithReservation(ctx, resource)).To(Succeed())

			reconciler := &ClusterReconciler{
				Client:         k8sClient,
				Scheme:         k8sClient.Scheme(),
				Dynamo:         &fakeDynamo{},
				RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "example.com", AWSRegion: "us-east-1"},
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			// Finalizer write emits a watch event that re-enqueues; no explicit requeue needed.
			Expect(result.RequeueAfter).To(BeZero())

			var updated seaplanev1alpha1.Cluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &updated)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(&updated, clusterFinalizer)).To(BeTrue())
		})

		It("should persist the reservation base domain on Cluster status", func() {
			cluster := newTestCluster(clusterName)
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			reconciler := &ClusterReconciler{Client: k8sClient}
			Expect(reconciler.persistBaseDomain(ctx, cluster, "f7a3.0.example.com")).To(Succeed())

			var updated seaplanev1alpha1.Cluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &updated)).To(Succeed())
			Expect(updated.Status.BaseDomain).To(Equal("f7a3.0.example.com"))
		})

		It("should release only the OIDC claim held by this Cluster", func() {
			config := &seaplanev1alpha1.OidcConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-oidc-config",
					Namespace: testNS,
					Labels:    map[string]string{claimedByClusterUIDLabel: "cluster-owner-uid"},
				},
				Spec: seaplanev1alpha1.OidcConfigSpec{
					Type:      seaplanev1alpha1.OidcConfigTypeManaged,
					IssuerUrl: "https://oidc.example.com/test",
				},
			}
			Expect(k8sClient.Create(ctx, config)).To(Succeed())

			cluster := newTestCluster(clusterName)
			cluster.UID = types.UID("cluster-owner-uid")
			cluster.Spec.OidcConfigID = string(config.UID)
			reconciler := &ClusterReconciler{Client: k8sClient}
			Expect(reconciler.releaseOidcConfigClaim(ctx, cluster)).To(Succeed())

			var updated seaplanev1alpha1.OidcConfig
			key := types.NamespacedName{Namespace: testNS, Name: config.Name}
			Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
			Expect(updated.Labels[claimedByClusterUIDLabel]).To(BeEmpty())

			if updated.Labels == nil {
				updated.Labels = make(map[string]string)
			}
			updated.Labels[claimedByClusterUIDLabel] = "another-cluster-uid"
			Expect(k8sClient.Update(ctx, &updated)).To(Succeed())
			Expect(reconciler.releaseOidcConfigClaim(ctx, cluster)).To(Succeed())
			Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
			Expect(updated.Labels[claimedByClusterUIDLabel]).To(Equal("another-cluster-uid"))

			cluster.Spec.OidcConfigID = "missing-oidc-config-uid"
			Expect(reconciler.releaseOidcConfigClaim(ctx, cluster)).To(Succeed())
		})

		It("should release owned claims and remove the finalizer when no resources remain", func() {
			config := &seaplanev1alpha1.OidcConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-oidc-config",
					Namespace: testNS,
				},
				Spec: seaplanev1alpha1.OidcConfigSpec{
					Type:      seaplanev1alpha1.OidcConfigTypeManaged,
					AccountID: "123456789012",
					IssuerUrl: "https://oidc.example.com/test",
				},
			}
			Expect(k8sClient.Create(ctx, config)).To(Succeed())

			cluster := newTestCluster(clusterName)
			cluster.Spec.DNSReservationID = ""
			cluster.Spec.OidcConfigID = string(config.UID)
			cluster.Finalizers = []string{clusterFinalizer}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			config.Labels = map[string]string{claimedByClusterUIDLabel: string(cluster.UID)}
			Expect(k8sClient.Update(ctx, config)).To(Succeed())

			reconciler := &ClusterReconciler{Client: k8sClient}
			result, err := reconciler.cleanupAndRemoveFinalizer(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			var updatedCluster seaplanev1alpha1.Cluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &updatedCluster)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(&updatedCluster, clusterFinalizer)).To(BeFalse())
			var updatedConfig seaplanev1alpha1.OidcConfig
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: config.Name}, &updatedConfig)).To(Succeed())
			Expect(updatedConfig.Labels[claimedByClusterUIDLabel]).To(BeEmpty())
		})

		It("should delete a DNSReservation with Delete policy before removing the Cluster finalizer", func() {
			reservation := &seaplanev1alpha1.DNSReservation{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-reservation",
					Namespace:  testNS,
					UID:        types.UID("test-reservation-uid"),
					Finalizers: []string{dnsReservationFinalizer},
					Labels:     map[string]string{accountIDLabel: "123456789012"},
				},
				Spec: seaplanev1alpha1.DNSReservationSpec{
					ClusterDeletionPolicy: seaplanev1alpha1.DNSReservationClusterDeletionPolicyDelete,
				},
				Status: seaplanev1alpha1.DNSReservationStatus{
					Phase:      seaplanev1alpha1.DNSReservationPhaseReady,
					BaseDomain: "f7a3.0.example.com",
				},
			}
			Expect(k8sClient.Create(ctx, reservation)).To(Succeed())

			cluster := newTestCluster(clusterName)
			cluster.Spec.DNSReservationID = string(reservation.UID)
			cluster.Finalizers = []string{clusterFinalizer}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			reservation.Labels[claimedByClusterUIDLabel] = string(cluster.UID)
			Expect(k8sClient.Update(ctx, reservation)).To(Succeed())

			reconciler := &ClusterReconciler{Client: k8sClient}
			result, err := reconciler.cleanupAndRemoveFinalizer(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Second))

			var deletingReservation seaplanev1alpha1.DNSReservation
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(reservation), &deletingReservation)).To(Succeed())
			Expect(deletingReservation.DeletionTimestamp.IsZero()).To(BeFalse())
		})

		It("should release and retain a customer DNSReservation by default", func() {
			reservation := &seaplanev1alpha1.DNSReservation{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "dns-" + clusterName,
					Namespace:  testNS,
					UID:        types.UID("customer-reservation-uid"),
					Finalizers: []string{dnsReservationFinalizer},
					Labels:     map[string]string{accountIDLabel: "123456789012"},
				},
				Status: seaplanev1alpha1.DNSReservationStatus{
					Phase:      seaplanev1alpha1.DNSReservationPhaseReady,
					BaseDomain: "f7a3.0.example.com",
				},
			}
			Expect(k8sClient.Create(ctx, reservation)).To(Succeed())
			reservation.Status.Phase = seaplanev1alpha1.DNSReservationPhaseReady
			reservation.Status.BaseDomain = "f7a3.0.example.com"
			Expect(k8sClient.Status().Update(ctx, reservation)).To(Succeed())
			var allocated seaplanev1alpha1.DNSReservation
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(reservation), &allocated)).To(Succeed())
			Expect(allocated.Status.BaseDomain).To(Equal("f7a3.0.example.com"))

			cluster := newTestCluster(clusterName)
			cluster.Spec.DNSReservationID = string(reservation.UID)
			cluster.Finalizers = []string{clusterFinalizer}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			reservation.Labels[claimedByClusterUIDLabel] = string(cluster.UID)
			Expect(k8sClient.Update(ctx, reservation)).To(Succeed())

			reconciler := &ClusterReconciler{Client: k8sClient}
			result, err := reconciler.cleanupAndRemoveFinalizer(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			var preserved seaplanev1alpha1.DNSReservation
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(reservation), &preserved)).To(Succeed())
			Expect(preserved.DeletionTimestamp.IsZero()).To(BeTrue())
			Expect(preserved.Labels[claimedByClusterUIDLabel]).To(BeEmpty())
			Expect(preserved.Status.BaseDomain).To(Equal("f7a3.0.example.com"))
		})

		It("should preserve a referenced DNSReservation after its Cluster claim is released", func() {
			reservation := &seaplanev1alpha1.DNSReservation{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "dns-" + clusterName,
					Namespace:  testNS,
					UID:        types.UID("released-reservation-uid"),
					Finalizers: []string{dnsReservationFinalizer},
					Labels:     map[string]string{accountIDLabel: "123456789012"},
				},
				Status: seaplanev1alpha1.DNSReservationStatus{
					Phase:      seaplanev1alpha1.DNSReservationPhaseReady,
					BaseDomain: "f7a3.0.example.com",
				},
			}
			Expect(k8sClient.Create(ctx, reservation)).To(Succeed())

			cluster := newTestCluster(clusterName)
			cluster.Spec.DNSReservationID = string(reservation.UID)
			cluster.Finalizers = []string{clusterFinalizer}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			reconciler := &ClusterReconciler{Client: k8sClient}
			result, err := reconciler.cleanupAndRemoveFinalizer(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			var preserved seaplanev1alpha1.DNSReservation
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(reservation), &preserved)).To(Succeed())
			Expect(preserved.DeletionTimestamp.IsZero()).To(BeTrue())
			Expect(preserved.Labels[claimedByClusterUIDLabel]).To(BeEmpty())
		})

		It("should preserve a referenced DNSReservation claimed by another Cluster", func() {
			reservation := &seaplanev1alpha1.DNSReservation{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "dns-" + clusterName,
					Namespace:  testNS,
					UID:        types.UID("foreign-reservation-uid"),
					Finalizers: []string{dnsReservationFinalizer},
					Labels: map[string]string{
						accountIDLabel:           "123456789012",
						claimedByClusterUIDLabel: "another-cluster-uid",
					},
				},
				Status: seaplanev1alpha1.DNSReservationStatus{
					Phase:      seaplanev1alpha1.DNSReservationPhaseReady,
					BaseDomain: "f7a3.0.example.com",
				},
			}
			Expect(k8sClient.Create(ctx, reservation)).To(Succeed())

			cluster := newTestCluster(clusterName)
			cluster.Spec.DNSReservationID = string(reservation.UID)
			cluster.Finalizers = []string{clusterFinalizer}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			reconciler := &ClusterReconciler{Client: k8sClient}
			result, err := reconciler.cleanupAndRemoveFinalizer(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			var preserved seaplanev1alpha1.DNSReservation
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(reservation), &preserved)).To(Succeed())
			Expect(preserved.DeletionTimestamp.IsZero()).To(BeTrue())
			Expect(preserved.Labels[claimedByClusterUIDLabel]).To(Equal("another-cluster-uid"))
		})

		It("should delete a legacy automatic DNSReservation without a stored reference", func() {
			cluster := newTestCluster(clusterName)
			cluster.Spec.DNSReservationID = ""
			cluster.Finalizers = []string{clusterFinalizer}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			reservation := &seaplanev1alpha1.DNSReservation{
				ObjectMeta: metav1.ObjectMeta{
					Name:       legacyAutomaticDNSReservationName(cluster.Name, string(cluster.UID)),
					Namespace:  testNS,
					UID:        types.UID("automatic-reservation-uid"),
					Finalizers: []string{dnsReservationFinalizer},
					Labels: map[string]string{
						accountIDLabel:           "123456789012",
						claimedByClusterUIDLabel: string(cluster.UID),
					},
				},
				Status: seaplanev1alpha1.DNSReservationStatus{
					Phase:      seaplanev1alpha1.DNSReservationPhaseReady,
					BaseDomain: "f7a3.0.example.com",
				},
			}
			Expect(k8sClient.Create(ctx, reservation)).To(Succeed())

			reconciler := &ClusterReconciler{Client: k8sClient}
			result, err := reconciler.cleanupAndRemoveFinalizer(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Second))

			var deleting seaplanev1alpha1.DNSReservation
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(reservation), &deleting)).To(Succeed())
			Expect(deleting.DeletionTimestamp.IsZero()).To(BeFalse())
		})

		It("should create and bind a DNSReservation when one is not supplied", func() {
			resource := newTestCluster(clusterName)
			resource.Spec.DNSReservationID = ""
			resource.Finalizers = []string{clusterFinalizer}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			reconciler := &ClusterReconciler{
				Client:         k8sClient,
				Scheme:         k8sClient.Scheme(),
				Dynamo:         &fakeDynamo{},
				RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "example.com", AWSRegion: "us-east-1"},
			}
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			var updated seaplanev1alpha1.Cluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &updated)).To(Succeed())
			Expect(updated.Spec.DNSReservationID).NotTo(BeEmpty())

			var reservation seaplanev1alpha1.DNSReservation
			reservationKey := types.NamespacedName{
				Namespace: testNS,
				Name:      automaticDNSReservationName(string(updated.UID)),
			}
			Expect(k8sClient.Get(ctx, reservationKey, &reservation)).To(Succeed())
			Expect(string(reservation.UID)).To(Equal(updated.Spec.DNSReservationID))
			Expect(reservation.Labels[accountIDLabel]).To(Equal("123456789012"))
			Expect(reservation.Labels[claimedByClusterUIDLabel]).To(Equal(string(updated.UID)))
			Expect(reservation.Spec.ClusterDeletionPolicy).To(Equal(seaplanev1alpha1.DNSReservationClusterDeletionPolicyDelete))
		})

		It("should set WaitingForPlacement when no Placement exists", func() {
			resource := newTestCluster(clusterName)
			Expect(createTestClusterWithReservation(ctx, resource)).To(Succeed())

			reconciler := &ClusterReconciler{
				Client:         k8sClient,
				Scheme:         k8sClient.Scheme(),
				Dynamo:         &fakeDynamo{},
				RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "example.com", AWSRegion: "us-east-1"},
			}

			// First reconcile adds finalizer.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			// Second reconcile checks for Placement.
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).NotTo(BeZero())

			var updated seaplanev1alpha1.Cluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(seaplanev1alpha1.ClusterPhaseWaitingForPlacement))
		})

		It("should create DynamoDB desires when Placement is Bound", func() {
			resource := newTestCluster(clusterName)
			Expect(createTestClusterWithReservation(ctx, resource)).To(Succeed())

			// Create a Bound Placement.
			placement := &seaplanev1alpha1.Placement{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName + ".placement",
					Namespace: testNS,
					Labels:    map[string]string{clusterUIDLabel: string(resource.UID)},
				},
				Spec: seaplanev1alpha1.PlacementSpec{
					ClusterName:       clusterName,
					ManagementCluster: "mc01",
				},
			}
			Expect(k8sClient.Create(ctx, placement)).To(Succeed())
			placement.Status.Phase = seaplanev1alpha1.PlacementPhaseBound
			Expect(k8sClient.Status().Update(ctx, placement)).To(Succeed())

			fd := &fakeDynamo{}
			reconciler := &ClusterReconciler{
				Client:         k8sClient,
				Scheme:         k8sClient.Scheme(),
				Dynamo:         fd,
				RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "example.com", AWSRegion: "us-east-1"},
			}

			// First reconcile: adds finalizer.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			// Second reconcile: creates desires.
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())

			// 6 cluster manifests → 6 ApplyDesires + 1 ReadDesire.
			Expect(fd.applyCount).To(Equal(6))
			Expect(fd.readCount).To(Equal(1))
			Expect(fd.reads[0].Spec.TargetItem.Name).To(Equal(render.HostedClusterName(resource)))
		})

		It("should create DynamoDB desires including the OIDC signing key ExternalSecret when OidcConfigID is set", func() {
			ensureNamespace(ctx, testNS)
			oc := &seaplanev1alpha1.OidcConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test-oidc-config", Namespace: testNS},
				Spec: seaplanev1alpha1.OidcConfigSpec{
					Type:             seaplanev1alpha1.OidcConfigTypeUnmanaged,
					IssuerUrl:        "https://oidc.example.com/test-oidc-config",
					SecretArn:        "arn:aws:secretsmanager:us-east-1:123456789012:secret:test",
					InstallerRoleArn: "arn:aws:iam::123456789012:role/installer",
					AccountID:        "123456789012",
				},
			}
			Expect(k8sClient.Create(ctx, oc)).To(Succeed())

			resource := newTestClusterWithOidcConfig(clusterName)
			resource.Spec.OidcConfigID = string(oc.UID)
			Expect(createTestClusterWithReservation(ctx, resource)).To(Succeed())

			// Create a Bound Placement.
			placement := &seaplanev1alpha1.Placement{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName + ".placement",
					Namespace: testNS,
					Labels:    map[string]string{clusterUIDLabel: string(resource.UID)},
				},
				Spec: seaplanev1alpha1.PlacementSpec{
					ClusterName:       clusterName,
					ManagementCluster: "mc01",
				},
			}
			Expect(k8sClient.Create(ctx, placement)).To(Succeed())
			placement.Status.Phase = seaplanev1alpha1.PlacementPhaseBound
			Expect(k8sClient.Status().Update(ctx, placement)).To(Succeed())

			fd := &fakeDynamo{}
			reconciler := &ClusterReconciler{
				Client:         k8sClient,
				Scheme:         k8sClient.Scheme(),
				Dynamo:         fd,
				RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "example.com", AWSRegion: "us-east-1"},
			}

			// First reconcile: adds finalizer.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			// Second reconcile: creates desires.
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())

			// 7 cluster manifests (6 base + oidc-signing-key ExternalSecret)
			// → 7 ApplyDesires + 1 ReadDesire.
			Expect(fd.applyCount).To(Equal(7))
			Expect(fd.readCount).To(Equal(1))
		})

		It("should switch all 6 desires to Type=Delete in-place, wait for confirmation, then remove finalizer", func() {
			resource := newTestCluster(clusterName)
			Expect(createTestClusterWithReservation(ctx, resource)).To(Succeed())

			// Create a Placement so the deletion path has something to clean up.
			placement := &seaplanev1alpha1.Placement{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName + ".placement",
					Namespace: testNS,
					Labels:    map[string]string{clusterUIDLabel: string(resource.UID)},
				},
				Spec: seaplanev1alpha1.PlacementSpec{
					ClusterName:       clusterName,
					ManagementCluster: "mc01",
				},
			}
			Expect(k8sClient.Create(ctx, placement)).To(Succeed())

			fd := &fakeDynamo{}
			reconciler := &ClusterReconciler{
				Client:         k8sClient,
				Scheme:         k8sClient.Scheme(),
				Dynamo:         fd,
				RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "example.com", AWSRegion: "us-east-1"},
			}

			// First reconcile: adds finalizer.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())

			// Set placementRef so the deletion path writes delete desires.
			var updated seaplanev1alpha1.Cluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &updated)).To(Succeed())
			updated.Status.PlacementRef = &seaplanev1alpha1.PlacementReference{
				Name:              clusterName + ".placement",
				ManagementCluster: "mc01",
			}
			Expect(k8sClient.Status().Update(ctx, &updated)).To(Succeed())

			// Delete the CR — sets DeletionTimestamp.
			Expect(k8sClient.Delete(ctx, &updated)).To(Succeed())

			// First deletion reconcile: switches all 6 desires to Type=Delete
			// in-place; no status yet → requeues.
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			deleteApplies := filterDeleteDesires(fd.applies)
			Expect(deleteApplies).To(HaveLen(6), "all 6 resources should be switched to Type=Delete")
			Expect(result.RequeueAfter).NotTo(BeZero(), "should requeue while waiting for deletion confirmation")

			// Placement should still exist (finalizer not removed yet).
			var p seaplanev1alpha1.Placement
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName + ".placement"}, &p)).To(Succeed())

			// Simulate kube-applier acknowledging the deletes but resources
			// still terminating (Successful=False, WaitingForDeletion).
			fd.applyStatus = &dynamo.ApplyDesireStatus{
				Conditions: []metav1.Condition{{
					Type:   dynamo.DesireConditionSuccessful,
					Status: metav1.ConditionFalse,
					Reason: "WaitingForDeletion",
				}},
			}

			// Second deletion reconcile: status exists but Successful!=True → requeues.
			result, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			deleteApplies = filterDeleteDesires(fd.applies)
			Expect(deleteApplies).To(HaveLen(12), "6 desires re-upserted on second pass")
			Expect(result.RequeueAfter).NotTo(BeZero(), "should requeue while resources still terminating")

			// Simulate all resources fully deleted (Successful=True).
			fd.applyStatus = &dynamo.ApplyDesireStatus{
				Conditions: []metav1.Condition{{
					Type:   dynamo.DesireConditionSuccessful,
					Status: metav1.ConditionTrue,
					Reason: "NoErrors",
				}},
			}

			// Third deletion reconcile: all 6 confirmed deleted → cleans up
			// desire specs and ReadDesire, deletes Placement, removes finalizer.
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			deleteApplies = filterDeleteDesires(fd.applies)
			Expect(deleteApplies).To(HaveLen(18), "6 desires re-upserted on third pass")

			// Verify the Placement was deleted.
			err = k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName + ".placement"}, &p)
			Expect(err).To(HaveOccurred())

			// Verify desire specs were cleaned up once at the end (not on every pass).
			// 6 ApplyDesire cleanups + 1 ReadDesire cleanup.
			applyCleanups, readCleanups := fd.countSpecCleanups()
			Expect(applyCleanups).To(Equal(6), "should clean up all 6 ApplyDesire specs once deletion confirmed")
			Expect(readCleanups).To(Equal(1), "should clean up ReadDesire spec")
		})

		It("should propagate HC status feedback and set Phase=Ready", func() {
			resource := newTestCluster(clusterName)
			Expect(createTestClusterWithReservation(ctx, resource)).To(Succeed())

			placement := &seaplanev1alpha1.Placement{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName + ".placement",
					Namespace: testNS,
					Labels:    map[string]string{clusterUIDLabel: string(resource.UID)},
				},
				Spec: seaplanev1alpha1.PlacementSpec{
					ClusterName:       clusterName,
					ManagementCluster: "mc01",
				},
			}
			Expect(k8sClient.Create(ctx, placement)).To(Succeed())
			placement.Status.Phase = seaplanev1alpha1.PlacementPhaseBound
			Expect(k8sClient.Status().Update(ctx, placement)).To(Succeed())

			fd := &fakeDynamo{
				applyStatus: &dynamo.ApplyDesireStatus{
					Conditions: []metav1.Condition{{
						Type: dynamo.DesireConditionSuccessful, Status: metav1.ConditionTrue, Reason: "NoErrors",
					}},
				},
				readStatus: &dynamo.ReadDesireStatus{
					KubeContent: &runtime.RawExtension{Raw: []byte(`{
						"status": {
							"conditions": [
								{"type": "Available", "status": "True", "reason": "HostedClusterAsExpected", "lastTransitionTime": "2026-06-25T10:00:00Z"},
								{"type": "Degraded", "status": "False", "reason": "AsExpected", "lastTransitionTime": "2026-06-25T10:00:00Z"}
							],
							"version": {
								"history": [{"version": "4.17.0"}]
							},
							"controlPlaneEndpoint": {
								"host": "api.my-cluster.example.com",
								"port": 6443
							}
						}
					}`)},
				},
			}
			reconciler := &ClusterReconciler{
				Client:         k8sClient,
				Scheme:         k8sClient.Scheme(),
				Dynamo:         fd,
				RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "example.com", AWSRegion: "us-east-1"},
			}

			// First reconcile: adds finalizer.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			// Second reconcile: creates desires + sets Provisioning.
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			// Third reconcile: phase is already Provisioning so setPhase is
			// skipped and Ready from updateStatusFromDynamo persists.
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())

			var updated seaplanev1alpha1.Cluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &updated)).To(Succeed())

			Expect(updated.Status.Phase).To(Equal(seaplanev1alpha1.ClusterPhaseReady))
			Expect(updated.Status.Version).To(Equal("4.17.0"))
			Expect(updated.Status.ControlPlaneEndpoint.Host).To(Equal("api.my-cluster.example.com"))
			Expect(updated.Status.ControlPlaneEndpoint.Port).To(Equal(int32(6443)))

			availCond := meta.FindStatusCondition(updated.Status.Conditions, "Available")
			Expect(availCond).NotTo(BeNil())
			Expect(availCond.Status).To(Equal(metav1.ConditionTrue))

			degradedCond := meta.FindStatusCondition(updated.Status.Conditions, "Degraded")
			Expect(degradedCond).NotTo(BeNil())
			Expect(degradedCond.Status).To(Equal(metav1.ConditionFalse))
		})

		It("should not set Phase=Ready when cluster is Degraded", func() {
			resource := newTestCluster(clusterName)
			Expect(createTestClusterWithReservation(ctx, resource)).To(Succeed())

			placement := &seaplanev1alpha1.Placement{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName + ".placement",
					Namespace: testNS,
					Labels:    map[string]string{clusterUIDLabel: string(resource.UID)},
				},
				Spec: seaplanev1alpha1.PlacementSpec{
					ClusterName:       clusterName,
					ManagementCluster: "mc01",
				},
			}
			Expect(k8sClient.Create(ctx, placement)).To(Succeed())
			placement.Status.Phase = seaplanev1alpha1.PlacementPhaseBound
			Expect(k8sClient.Status().Update(ctx, placement)).To(Succeed())

			fd := &fakeDynamo{
				applyStatus: &dynamo.ApplyDesireStatus{
					Conditions: []metav1.Condition{{
						Type: dynamo.DesireConditionSuccessful, Status: metav1.ConditionTrue, Reason: "NoErrors",
					}},
				},
				readStatus: &dynamo.ReadDesireStatus{
					KubeContent: &runtime.RawExtension{Raw: []byte(`{
						"status": {
							"conditions": [
								{"type": "Available", "status": "True", "reason": "HostedClusterAsExpected", "lastTransitionTime": "2026-06-25T10:00:00Z"},
								{"type": "Degraded", "status": "True", "reason": "ComponentFailing", "lastTransitionTime": "2026-06-25T10:00:00Z"}
							]
						}
					}`)},
				},
			}
			reconciler := &ClusterReconciler{
				Client:         k8sClient,
				Scheme:         k8sClient.Scheme(),
				Dynamo:         fd,
				RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "example.com", AWSRegion: "us-east-1"},
			}

			// Finalizer + desires + third reconcile (same as Ready test).
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())

			var updated seaplanev1alpha1.Cluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &updated)).To(Succeed())
			Expect(updated.Status.Phase).NotTo(Equal(seaplanev1alpha1.ClusterPhaseReady))
		})

		It("should delete an expired cluster", func() {
			resource := newTestCluster(clusterName)
			expiry := metav1.NewTime(time.Now().Add(-1 * time.Minute))
			resource.Spec.ExpirationTimestamp = &expiry
			Expect(createTestClusterWithReservation(ctx, resource)).To(Succeed())

			reconciler := &ClusterReconciler{
				Client:         k8sClient,
				Scheme:         k8sClient.Scheme(),
				Dynamo:         &fakeDynamo{},
				RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "example.com", AWSRegion: "us-east-1"},
			}

			// First reconcile adds finalizer.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile detects expiration and deletes.
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())

			// Cluster should have a DeletionTimestamp set.
			var updated seaplanev1alpha1.Cluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: clusterName}, &updated)).To(Succeed())
			Expect(updated.DeletionTimestamp.IsZero()).To(BeFalse())
		})

		It("should requeue at expiration time when it is sooner than statusRefreshDelay", func() {
			resource := newTestCluster(clusterName)
			expiry := metav1.NewTime(time.Now().Add(30 * time.Second))
			resource.Spec.ExpirationTimestamp = &expiry
			Expect(createTestClusterWithReservation(ctx, resource)).To(Succeed())

			placement := &seaplanev1alpha1.Placement{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName + ".placement",
					Namespace: testNS,
					Labels:    map[string]string{clusterUIDLabel: string(resource.UID)},
				},
				Spec: seaplanev1alpha1.PlacementSpec{
					ClusterName:       clusterName,
					ManagementCluster: "mc01",
				},
			}
			Expect(k8sClient.Create(ctx, placement)).To(Succeed())
			placement.Status.Phase = seaplanev1alpha1.PlacementPhaseBound
			Expect(k8sClient.Status().Update(ctx, placement)).To(Succeed())

			reconciler := &ClusterReconciler{
				Client:         k8sClient,
				Scheme:         k8sClient.Scheme(),
				Dynamo:         &fakeDynamo{},
				RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "example.com", AWSRegion: "us-east-1"},
			}

			// First reconcile: adds finalizer.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			// Second reconcile: creates desires + returns requeue.
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: clusterName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(result.RequeueAfter).To(BeNumerically("<", statusRefreshDelay))
		})

		It("should handle not-found gracefully", func() {
			reconciler := &ClusterReconciler{
				Client:         k8sClient,
				Scheme:         k8sClient.Scheme(),
				Dynamo:         &fakeDynamo{},
				RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "example.com", AWSRegion: "us-east-1"},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: "does-not-exist"},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

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
			Namespace: "account-123456789012",
			Labels:    map[string]string{accountIDLabel: "123456789012"},
		},
		Spec: seaplanev1alpha1.ClusterSpec{
			AccountID:         "123456789012",
			CreatorARN:        "arn:aws:iam::123456789012:user/admin",
			DNSReservationID:  "test-reservation-uid",
			HostedClusterName: name,
			HostedCluster: seaplanev1alpha1.HostedClusterSpecPassthrough{
				Release:    hypershiftv1beta1.Release{Image: "quay.io/openshift-release-dev/ocp-release:4.17.0-ec.2-x86_64"},
				IssuerURL:  "https://oidc.example.com/cluster-01",
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
					{Service: hypershiftv1beta1.APIServer, ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{Type: hypershiftv1beta1.Route}},
					{Service: hypershiftv1beta1.OAuthServer, ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{Type: hypershiftv1beta1.Route}},
					{Service: hypershiftv1beta1.Konnectivity, ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{Type: hypershiftv1beta1.Route}},
					{Service: hypershiftv1beta1.Ignition, ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{Type: hypershiftv1beta1.Route}},
				},
				Platform: hypershiftv1beta1.PlatformSpec{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS: &hypershiftv1beta1.AWSPlatformSpec{
						Region: "us-east-1",
						CloudProviderConfig: &hypershiftv1beta1.AWSCloudProviderConfig{
							VPC:  "vpc-abc123",
							Zone: "us-east-1a",
							Subnet: &hypershiftv1beta1.AWSResourceReference{
								ID: ptr.To("subnet-1,subnet-2"),
							},
						},
						RolesRef: hypershiftv1beta1.AWSRolesRef{
							ControlPlaneOperatorARN: "arn:aws:iam::123456789012:role/cpo",
							IngressARN:              "arn:aws:iam::123456789012:role/ingress",
							ImageRegistryARN:        "arn:aws:iam::123456789012:role/registry",
							KubeCloudControllerARN:  "arn:aws:iam::123456789012:role/kccm",
							NodePoolManagementARN:   "arn:aws:iam::123456789012:role/npm",
							NetworkARN:              "arn:aws:iam::123456789012:role/network",
							StorageARN:              "arn:aws:iam::123456789012:role/storage",
						},
					},
				},
			},
		},
	}
}

func createTestClusterWithReservation(ctx context.Context, cluster *seaplanev1alpha1.Cluster) error {
	accountID := cluster.Spec.AccountID
	if accountID == "" {
		return fmt.Errorf("test Cluster %s has no account ID", cluster.Name)
	}
	cluster.Namespace = accountNamespace(accountID)
	ensureNamespace(ctx, cluster.Namespace)
	ensureNamespace(ctx, dnsShardNamespace(defaultDNSShard))
	if cluster.Labels == nil {
		cluster.Labels = make(map[string]string)
	}
	cluster.Labels[accountIDLabel] = accountID

	reservation := &seaplanev1alpha1.DNSReservation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dns-" + cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    map[string]string{accountIDLabel: accountID},
		},
		Spec: seaplanev1alpha1.DNSReservationSpec{},
	}
	if err := k8sClient.Create(ctx, reservation); err != nil {
		return err
	}
	reservation.Status.Phase = seaplanev1alpha1.DNSReservationPhaseReady
	reservation.Status.BaseDomain = "f7a3.0.example.com"
	reservation.Status.InternalHostedClusterName = seaplanev1alpha1.HostedClusterNameFromDNSReservation(reservation)
	if err := k8sClient.Status().Update(ctx, reservation); err != nil {
		return err
	}
	cluster.Spec.DNSReservationID = string(reservation.UID)
	if err := k8sClient.Create(ctx, cluster); err != nil {
		return err
	}
	// Rendered HostedClusters are namespaced by cluster UID. The PostgreSQL
	// client does not require the namespace to exist, but envtest's Kubernetes
	// API server does.
	ensureNamespace(ctx, render.ManagementNamespace(string(cluster.UID)))
	reservation.Labels[claimedByClusterUIDLabel] = string(cluster.UID)
	if err := k8sClient.Update(ctx, reservation); err != nil {
		return err
	}

	if cluster.Spec.OidcConfigID != "" {
		var configs seaplanev1alpha1.OidcConfigList
		if err := k8sClient.List(ctx, &configs, client.InNamespace(cluster.Namespace)); err != nil {
			return err
		}
		claimed := false
		for i := range configs.Items {
			config := &configs.Items[i]
			if string(config.UID) != cluster.Spec.OidcConfigID {
				continue
			}
			if config.Labels == nil {
				config.Labels = make(map[string]string)
			}
			config.Labels[claimedByClusterUIDLabel] = string(cluster.UID)
			if err := k8sClient.Update(ctx, config); err != nil {
				return err
			}
			claimed = true
			break
		}
		if !claimed {
			return fmt.Errorf("test OidcConfig UID %s not found in %s", cluster.Spec.OidcConfigID, cluster.Namespace)
		}
	}
	cluster.Status.BaseDomain = reservation.Status.BaseDomain
	return k8sClient.Status().Update(ctx, cluster)
}

// newTestClusterWithOidcConfig returns a cluster fixture using the
// OidcConfig-backed issuer path (OidcConfigID set).
func newTestClusterWithOidcConfig(name string) *seaplanev1alpha1.Cluster {
	cluster := newTestCluster(name)
	cluster.Spec.OidcConfigID = "test-oidc-config"
	return cluster
}

// filterDeleteDesires returns ApplyDesires that have Type=Delete.
func filterDeleteDesires(applies []*dynamo.ApplyDesire) []*dynamo.ApplyDesire {
	var out []*dynamo.ApplyDesire
	for _, a := range applies {
		if a.Spec.Type == dynamo.ApplyDesireTypeDelete {
			out = append(out, a)
		}
	}
	return out
}
