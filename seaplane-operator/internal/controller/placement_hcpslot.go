package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/render"
)

// An HCP slot claim is an Index named after the zero-padded slot number (e.g. "07") in hcp-slots-<mc>,
// labeled with the Cluster UID; the primary key admits one claim per slot.
const (
	hcpSlotNamespacePrefix = "hcp-slots-"
	// hcpSlotFinalizer on a Placement releases its slot claim on deletion.
	hcpSlotFinalizer = "seaplane.io/hcp-slot"
)

func hcpSlotNamespace(managementCluster string) string {
	return hcpSlotNamespacePrefix + managementCluster
}

// claimSlot returns the slot the Cluster already holds on any management cluster,
// else claims one on the first management cluster with a free slot.
// It returns slot 0 when every management cluster is full.
func (r *PlacementReconciler) claimSlot(ctx context.Context, cluster *seaplanev1alpha1.Cluster) (string, int32, error) {
	if mc, slot, err := r.heldHCPSlot(ctx, cluster.UID); err != nil || slot > 0 {
		return mc, slot, err
	}

	var list seaplanev1alpha1.ManagementClusterList
	if err := r.List(ctx, &list); err != nil {
		return "", 0, fmt.Errorf("list management clusters: %w", err)
	}
	found := false
	for i := range list.Items {
		mc := list.Items[i].Name
		// Skip fake MCs created by e2e tests.
		if strings.HasPrefix(mc, "test-mc") {
			continue
		}
		found = true
		slot, err := r.claimHCPSlot(ctx, cluster, mc)
		if err != nil {
			return "", 0, err
		}
		if slot > 0 {
			return mc, slot, nil
		}
	}
	if !found {
		return "", 0, fmt.Errorf("no management clusters configured")
	}
	return "", 0, nil
}

// heldHCPSlot returns a slot claim the Cluster already holds, e.g. from a placement
// interrupted before its Placement was written.
func (r *PlacementReconciler) heldHCPSlot(ctx context.Context, clusterUID types.UID) (string, int32, error) {
	var claims seaplanev1alpha1.IndexList
	if err := r.List(ctx, &claims, client.MatchingLabels{ownerUIDLabel: string(clusterUID)}); err != nil {
		return "", 0, fmt.Errorf("list HCP slot claims of Cluster UID %s: %w", clusterUID, err)
	}
	var heldMC string
	var held int32
	for i := range claims.Items {
		mc, ok := strings.CutPrefix(claims.Items[i].Namespace, hcpSlotNamespacePrefix)
		slot := parseHCPSlot(claims.Items[i].Name)
		if !ok || slot == 0 {
			continue
		}
		// Pick deterministically so racing attempts converge on the same claim.
		if held == 0 || mc < heldMC || (mc == heldMC && slot < held) {
			heldMC, held = mc, slot
		}
	}
	return heldMC, held, nil
}

// claimHCPSlot claims the lowest free slot on mc. It returns 0 when mc is full.
func (r *PlacementReconciler) claimHCPSlot(ctx context.Context, cluster *seaplanev1alpha1.Cluster, mc string) (int32, error) {
	namespace := hcpSlotNamespace(mc)
	var claims seaplanev1alpha1.IndexList
	if err := r.List(ctx, &claims, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("list HCP slot claims on %s: %w", mc, err)
	}
	taken := make(map[string]bool, len(claims.Items))
	for i := range claims.Items {
		claim := &claims.Items[i]
		// A racing attempt for this Cluster claimed here since heldHCPSlot ran.
		if claim.Labels[ownerUIDLabel] == string(cluster.UID) {
			if slot := parseHCPSlot(claim.Name); slot > 0 {
				return slot, nil
			}
		}
		taken[claim.Name] = true
	}

	for slot := int32(1); slot <= int32(r.HCPSlotCount); slot++ {
		name := render.HCPSlotName(slot)
		if taken[name] {
			continue
		}
		claim := &seaplanev1alpha1.Index{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{ownerUIDLabel: string(cluster.UID)},
			},
		}
		if err := r.Create(ctx, claim); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return 0, fmt.Errorf("claim HCP slot %s on %s: %w", name, mc, err)
			}
			// Taken since the list; keep it if a racing attempt for this Cluster took it.
			var existing seaplanev1alpha1.Index
			if err := r.Get(ctx, client.ObjectKeyFromObject(claim), &existing); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return 0, fmt.Errorf("get HCP slot claim %s on %s: %w", name, mc, err)
			}
			if existing.Labels[ownerUIDLabel] == string(cluster.UID) {
				return slot, nil
			}
			continue
		}
		return slot, nil
	}
	return 0, nil
}

// releaseHCPSlot deletes the claim on (mc, slot) if ownerUID still holds it.
func (r *PlacementReconciler) releaseHCPSlot(ctx context.Context, mc string, slot int32, ownerUID types.UID) error {
	if mc == "" || slot < 1 || ownerUID == "" {
		return nil
	}
	var claim seaplanev1alpha1.Index
	key := types.NamespacedName{Namespace: hcpSlotNamespace(mc), Name: render.HCPSlotName(slot)}
	if err := r.Get(ctx, key, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get HCP slot claim %s: %w", key, err)
	}
	if claim.Labels[ownerUIDLabel] != string(ownerUID) {
		return nil
	}
	// Delete carries the read uid and resourceVersion, so a re-claimed slot gets Conflict.
	if err := r.Delete(ctx, &claim); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("release HCP slot claim %s: %w", key, err)
	}
	return nil
}

func parseHCPSlot(name string) int32 {
	if len(name) != 2 || strings.TrimLeft(name, "0123456789") != "" {
		return 0
	}
	slot, err := strconv.ParseInt(name, 10, 32)
	if err != nil {
		return 0
	}
	return int32(slot)
}
