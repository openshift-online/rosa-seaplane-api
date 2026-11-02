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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/dynamo"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/render"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

var _ = Describe("NodePool Controller", func() {
	Context("When reconciling a NodePool", func() {
		const (
			clusterName  = "test-np-cluster"
			nodePoolName = "test-np-cluster.workers"
			testNS       = "account-123456789012"
		)

		ctx := context.Background()

		BeforeEach(func() {
			ensureNamespace(ctx, testNS)
		})

		AfterEach(func() {
			np := &seaplanev1alpha1.NodePool{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: nodePoolName}, np); err == nil {
				controllerutil.RemoveFinalizer(np, nodePoolFinalizer)
				_ = k8sClient.Update(ctx, np)
				_ = k8sClient.Delete(ctx, np)
			}
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

		It("should add a finalizer on first reconcile", func() {
			cluster := newTestCluster(clusterName)
			createNodePoolTestCluster(ctx, cluster)

			np := newTestNodePool(cluster)
			Expect(k8sClient.Create(ctx, np)).To(Succeed())

			reconciler := &NodePoolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dynamo: &fakeDynamo{},
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			Expect(err).NotTo(HaveOccurred())
			// Finalizer write emits a watch event that re-enqueues; no explicit requeue needed.
			Expect(result.RequeueAfter).To(BeZero())

			var updated seaplanev1alpha1.NodePool
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: nodePoolName}, &updated)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(&updated, nodePoolFinalizer)).To(BeTrue())
		})

		It("should set WaitingForCluster when parent Cluster has no PlacementRef", func() {
			cluster := newTestCluster(clusterName)
			createNodePoolTestCluster(ctx, cluster)

			np := newTestNodePool(cluster)
			Expect(k8sClient.Create(ctx, np)).To(Succeed())

			fd := &fakeDynamo{}
			reconciler := &NodePoolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dynamo: fd,
			}

			// First reconcile: adds finalizer.
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			// Second reconcile: cluster exists but no PlacementRef.
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).NotTo(BeZero())
			Expect(fd.applyCount).To(Equal(0))
		})

		It("should create ApplyDesire when parent Cluster has a Bound Placement", func() {
			cluster := newTestCluster(clusterName)
			createNodePoolTestCluster(ctx, cluster)

			// Set placementRef on the cluster status.
			cluster.Status.PlacementRef = &seaplanev1alpha1.PlacementReference{
				Name:              clusterName + ".placement",
				ManagementCluster: "mc01",
			}
			Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())

			np := newTestNodePool(cluster)
			Expect(k8sClient.Create(ctx, np)).To(Succeed())

			fd := &fakeDynamo{}
			reconciler := &NodePoolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dynamo: fd,
			}

			// First reconcile: adds finalizer.
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			// Second reconcile: creates desire.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(fd.applyCount).To(Equal(1))
		})

		It("should flip desire to Type=Delete in-place, wait for confirmation, and remove finalizer on deletion", func() {
			cluster := newTestCluster(clusterName)
			createNodePoolTestCluster(ctx, cluster)

			cluster.Status.PlacementRef = &seaplanev1alpha1.PlacementReference{
				Name:              clusterName + ".placement",
				ManagementCluster: "mc01",
			}
			Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())

			np := newTestNodePool(cluster)
			Expect(k8sClient.Create(ctx, np)).To(Succeed())

			fd := &fakeDynamo{}
			reconciler := &NodePoolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dynamo: fd,
			}

			// Add finalizer.
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})

			// Delete the NodePool.
			var toDelete seaplanev1alpha1.NodePool
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: nodePoolName}, &toDelete)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &toDelete)).To(Succeed())

			// First deletion reconcile: flips desire to Type=Delete in-place;
			// no status yet → requeues. No pre-deletion of ApplyDesire spec.
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(fd.deletedSpecs).To(BeEmpty(), "should not delete specs before deletion is confirmed")
			deleteApplies := filterDeleteDesires(fd.applies)
			Expect(deleteApplies).To(HaveLen(1))
			Expect(deleteApplies[0].Spec.TargetItem.Resource).To(Equal("nodepools"))
			// documentID must be the same as the SSA desire (same taskKey, no "+"-delete"" suffix).
			Expect(deleteApplies[0].DynamoDBMetadata.DocumentID).NotTo(BeEmpty())
			Expect(result.RequeueAfter).NotTo(BeZero(), "should requeue while waiting for confirmation")

			// Simulate kube-applier confirming deletion (Successful=True).
			fd.applyStatus = &dynamo.ApplyDesireStatus{
				Conditions: []metav1.Condition{{
					Type:   dynamo.DesireConditionSuccessful,
					Status: metav1.ConditionTrue,
					Reason: "NoErrors",
				}},
			}

			// Second deletion reconcile: confirmation found → cleans up specs and removes finalizer.
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify NodePool is gone (finalizer removed → k8s deletes it).
			err = k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: nodePoolName}, &seaplanev1alpha1.NodePool{})
			Expect(err).To(HaveOccurred())

			// ApplyDesire spec and ReadDesire spec cleaned up after confirmation.
			applyCleanups, readCleanups := fd.countSpecCleanups()
			Expect(applyCleanups).To(Equal(1), "should clean up ApplyDesire spec after deletion confirmed")
			Expect(readCleanups).To(Equal(1), "should clean up ReadDesire spec after deletion confirmed")
		})

		It("should create ReadDesire alongside ApplyDesire", func() {
			cluster := newTestCluster(clusterName)
			createNodePoolTestCluster(ctx, cluster)

			cluster.Status.PlacementRef = &seaplanev1alpha1.PlacementReference{
				Name:              clusterName + ".placement",
				ManagementCluster: "mc01",
			}
			Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())

			np := newTestNodePool(cluster)
			Expect(k8sClient.Create(ctx, np)).To(Succeed())

			fd := &fakeDynamo{}
			reconciler := &NodePoolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dynamo: fd,
			}

			// First reconcile: adds finalizer.
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			// Second reconcile: creates desires.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(fd.applyCount).To(Equal(1))
			Expect(fd.readCount).To(Equal(1))
			Expect(fd.reads[0].Spec.TargetItem.Resource).To(Equal("nodepools"))
		})

		It("should set Phase=Ready when ReadDesire reports Ready=True", func() {
			cluster := newTestCluster(clusterName)
			createNodePoolTestCluster(ctx, cluster)

			cluster.Status.PlacementRef = &seaplanev1alpha1.PlacementReference{
				Name:              clusterName + ".placement",
				ManagementCluster: "mc01",
			}
			Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())

			np := newTestNodePool(cluster)
			Expect(k8sClient.Create(ctx, np)).To(Succeed())

			fd := &fakeDynamo{
				applyStatus: &dynamo.ApplyDesireStatus{
					AppliedResourceGeneration: 1,
					Conditions: []metav1.Condition{{
						Type:   dynamo.DesireConditionSuccessful,
						Status: metav1.ConditionTrue,
						Reason: "NoErrors",
					}},
				},
				readStatus: &dynamo.ReadDesireStatus{
					KubeContent: &runtime.RawExtension{Raw: []byte(`{"status":{"conditions":[{"type":"Ready","status":"True","reason":"AsExpected","message":"All nodes ready","lastTransitionTime":"2026-06-25T10:00:00Z"}]}}`)},
				},
			}
			reconciler := &NodePoolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dynamo: fd,
			}

			// First reconcile: adds finalizer.
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			// Second reconcile: creates desires + reads status.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			Expect(err).NotTo(HaveOccurred())

			var updated seaplanev1alpha1.NodePool
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: nodePoolName}, &updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(seaplanev1alpha1.NodePoolPhaseReady))
		})

		It("should persist observed replicas from ReadDesire, preserving absent versus zero", func() {
			np := newTestNodePool()
			staleReplicas := int32(9)
			np.Status.Replicas = &staleReplicas
			Expect(k8sClient.Create(ctx, np)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: nodePoolName}, np)).To(Succeed())
			np.Status.Replicas = &staleReplicas
			Expect(k8sClient.Status().Update(ctx, np)).To(Succeed())

			reconciler := &NodePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			fd := &fakeDynamo{}
			absentReplicas := &dynamo.ReadDesireStatus{
				KubeContent: &runtime.RawExtension{Raw: []byte(`{"spec":{"replicas":2},"status":{}}`)},
			}
			fd.readStatus = absentReplicas
			reconciler.Dynamo = fd
			reconciler.updateStatusFromDynamo(ctx, np, "", DesireStatusEntry{}, "")

			var updated seaplanev1alpha1.NodePool
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: nodePoolName}, &updated)).To(Succeed())
			Expect(updated.Spec.NodePool.Replicas).NotTo(BeNil())
			Expect(*updated.Spec.NodePool.Replicas).To(Equal(int32(2)))
			Expect(updated.Status.Replicas).To(BeNil(), "missing observed status must not fall back to desired replicas")

			fd.readStatus = &dynamo.ReadDesireStatus{
				KubeContent: &runtime.RawExtension{Raw: []byte(`{"status":{"replicas":0}}`)},
			}
			reconciler.updateStatusFromDynamo(ctx, &updated, "", DesireStatusEntry{}, "")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: nodePoolName}, &updated)).To(Succeed())
			Expect(updated.Status.Replicas).NotTo(BeNil())
			Expect(*updated.Status.Replicas).To(Equal(int32(0)))
		})

		It("should clean up ApplyDesire and ReadDesire specs after deletion confirmed", func() {
			cluster := newTestCluster(clusterName)
			createNodePoolTestCluster(ctx, cluster)

			cluster.Status.PlacementRef = &seaplanev1alpha1.PlacementReference{
				Name:              clusterName + ".placement",
				ManagementCluster: "mc01",
			}
			Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())

			np := newTestNodePool(cluster)
			Expect(k8sClient.Create(ctx, np)).To(Succeed())

			fd := &fakeDynamo{
				applyStatus: &dynamo.ApplyDesireStatus{
					Conditions: []metav1.Condition{{
						Type:   dynamo.DesireConditionSuccessful,
						Status: metav1.ConditionTrue,
						Reason: "NoErrors",
					}},
				},
			}
			reconciler := &NodePoolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Dynamo: fd,
			}

			// Add finalizer.
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})

			// Delete the NodePool.
			var toDelete seaplanev1alpha1.NodePool
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: nodePoolName}, &toDelete)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &toDelete)).To(Succeed())

			// Reconcile deletion: delete desire confirmed immediately (applyStatus pre-set).
			// Should flip desire, see Successful=True, then clean up both specs.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			Expect(err).NotTo(HaveOccurred())

			// 1 ApplyDesire cleanup + 1 ReadDesire cleanup, both after confirmation.
			applyCleanups, readCleanups := fd.countSpecCleanups()
			Expect(applyCleanups).To(Equal(1), "should clean up ApplyDesire spec")
			Expect(readCleanups).To(Equal(1), "should clean up ReadDesire spec")
		})

		It("should remove the finalizer when a deleting NodePool has no Cluster controller owner", func() {
			nodePool := newTestNodePool()
			nodePool.Finalizers = []string{nodePoolFinalizer}
			Expect(k8sClient.Create(ctx, nodePool)).To(Succeed())
			Expect(k8sClient.Delete(ctx, nodePool)).To(Succeed())

			fd := &fakeDynamo{}
			reconciler := &NodePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dynamo: fd}
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			Expect(fd.applyCount).To(Equal(0))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: nodePoolName}, &seaplanev1alpha1.NodePool{}))).To(BeTrue())
		})

		It("should remove the finalizer when the referenced Cluster no longer exists", func() {
			cluster := newTestCluster(clusterName)
			createNodePoolTestCluster(ctx, cluster)

			nodePool := newTestNodePool(cluster)
			nodePool.Finalizers = []string{nodePoolFinalizer}
			Expect(k8sClient.Create(ctx, nodePool)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
			Expect(k8sClient.Delete(ctx, nodePool)).To(Succeed())

			fd := &fakeDynamo{}
			reconciler := &NodePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dynamo: fd}
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			Expect(fd.applyCount).To(Equal(0))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: nodePoolName}, &seaplanev1alpha1.NodePool{}))).To(BeTrue())
		})

		It("should remove the finalizer without MC cleanup when the Cluster has no PlacementRef", func() {
			cluster := newTestCluster(clusterName)
			createNodePoolTestCluster(ctx, cluster)

			nodePool := newTestNodePool(cluster)
			nodePool.Finalizers = []string{nodePoolFinalizer}
			Expect(k8sClient.Create(ctx, nodePool)).To(Succeed())
			Expect(k8sClient.Delete(ctx, nodePool)).To(Succeed())

			fd := &fakeDynamo{}
			reconciler := &NodePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dynamo: fd}
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			Expect(fd.applyCount).To(Equal(0))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: nodePoolName}, &seaplanev1alpha1.NodePool{}))).To(BeTrue())
		})

		It("should propagate non-NotFound Cluster lookup errors during deletion", func() {
			cluster := newTestCluster(clusterName)
			createNodePoolTestCluster(ctx, cluster)

			nodePool := newTestNodePool(cluster)
			nodePool.Finalizers = []string{nodePoolFinalizer}
			Expect(k8sClient.Create(ctx, nodePool)).To(Succeed())
			Expect(k8sClient.Delete(ctx, nodePool)).To(Succeed())

			lookupErr := errors.New("simulated Cluster lookup failure")
			reconciler := &NodePoolReconciler{
				Client: nodePoolClusterGetErrorClient{Client: k8sClient, err: lookupErr},
				Scheme: k8sClient.Scheme(),
				Dynamo: &fakeDynamo{},
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: testNS, Name: nodePoolName},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("get parent Cluster for NodePool deletion"))
			Expect(err.Error()).To(ContainSubstring(lookupErr.Error()))
		})
	})
})

type nodePoolClusterGetErrorClient struct {
	client.Client
	err error
}

func (c nodePoolClusterGetErrorClient) Get(ctx context.Context, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*seaplanev1alpha1.Cluster); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func newTestNodePool(parents ...*seaplanev1alpha1.Cluster) *seaplanev1alpha1.NodePool {
	nodePool := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-np-cluster.workers",
			Namespace: "account-123456789012",
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
				ClusterName: "test-np-cluster",
				Replicas:    ptr.To(int32(2)),
				Management: hypershiftv1beta1.NodePoolManagement{
					AutoRepair:  true,
					UpgradeType: hypershiftv1beta1.UpgradeTypeReplace,
				},
				Release: hypershiftv1beta1.Release{
					Image: "quay.io/openshift-release-dev/ocp-release:4.17.0-ec.2-x86_64",
				},
				Platform: hypershiftv1beta1.NodePoolPlatform{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS: &hypershiftv1beta1.AWSNodePoolPlatform{
						InstanceType:    "m6a.xlarge",
						RootVolume:      &hypershiftv1beta1.Volume{Size: 120, Type: "gp3"},
						InstanceProfile: "worker-profile",
						Subnet: hypershiftv1beta1.AWSResourceReference{
							ID: ptr.To("subnet-abc123"),
						},
						SecurityGroups: []hypershiftv1beta1.AWSResourceReference{
							{ID: ptr.To("sg-abc123")},
						},
					},
				},
			},
		},
	}
	if len(parents) > 0 && parents[0] != nil {
		parent := parents[0]
		nodePool.Labels = map[string]string{clusterUIDLabel: string(parent.UID)}
		nodePool.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(parent, seaplanev1alpha1.GroupVersion.WithKind("Cluster"))}
	}
	return nodePool
}

func createNodePoolTestCluster(ctx context.Context, cluster *seaplanev1alpha1.Cluster) {
	Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	// Rendered NodePools are stored in a namespace derived from the Cluster UID.
	// envtest requires that namespace to exist before the rendered resource is created.
	ensureNamespace(ctx, render.ManagementNamespace(string(cluster.UID)))
}
