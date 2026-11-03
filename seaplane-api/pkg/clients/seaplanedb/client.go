package seaplanedb

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	seaplanedb "github.com/openshift-online/rosa-seaplane-api/seaplane-db"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
)

const accountIDLabel = seaplanev1alpha1.AccountIDLabel
const clusterUIDLabel = seaplanev1alpha1.ClusterUIDLabel

const spotQueueClaimTimeout = 10 * time.Minute
const spotQueueOwnerPrefix = "cluster-"

// Client wraps a pgruntime client.Client for CRUD on seaplane resources.
type Client struct {
	client client.Client
	close  func()
	logger *slog.Logger
}

// NewClient creates a Client backed by seaplanedb.NewClient.
// The direct client is never sharded and sees all data.
func NewClient(ctx context.Context, dsn string, logger *slog.Logger) (*Client, error) {
	scheme := runtime.NewScheme()
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register seaplane scheme: %w", err)
	}
	// ConfigMaps back the temporary account allowlist.
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register core scheme: %w", err)
	}

	c, cleanup, err := seaplanedb.NewClient(seaplanedb.Options{
		Scheme: scheme,
		DSN:    dsn,
	})
	if err != nil {
		return nil, fmt.Errorf("create pgruntime client: %w", err)
	}

	return &Client{client: c, close: cleanup, logger: logger}, nil
}

// NewClientFrom wraps an existing client.Client (useful for testing with fakes).
func NewClientFrom(c client.Client, logger *slog.Logger) *Client {
	return &Client{client: c, close: func() {}, logger: logger}
}

// Close releases the underlying connection pool.
func (c *Client) Close() {
	c.close()
}

// --- Cluster operations ---

// CreateCluster creates a Cluster in the account namespace from ctx.
func (c *Client) CreateCluster(ctx context.Context, cluster *seaplanev1alpha1.Cluster) error {
	accountID, err := validateOwnedCandidate(ctx, cluster, cluster.Spec.AccountID)
	if err != nil {
		return err
	}
	cluster.Spec.AccountID = accountID
	return c.client.Create(ctx, cluster)
}

// GetCluster retrieves a Cluster by name in the account namespace from ctx.
func (c *Client) GetCluster(ctx context.Context, name string) (*seaplanev1alpha1.Cluster, error) {
	accountID := middleware.GetAccountID(ctx)
	var cluster seaplanev1alpha1.Cluster
	err := c.client.Get(ctx, k8stypes.NamespacedName{
		Namespace: accountNamespace(accountID),
		Name:      name,
	}, &cluster)
	if err != nil {
		return nil, err
	}
	return &cluster, nil
}

// ListClusters lists Clusters in the account namespace from ctx.
func (c *Client) ListClusters(ctx context.Context) (*seaplanev1alpha1.ClusterList, error) {
	accountID := middleware.GetAccountID(ctx)
	var list seaplanev1alpha1.ClusterList
	err := c.client.List(ctx, &list, client.InNamespace(accountNamespace(accountID)))
	if err != nil {
		return nil, err
	}
	return &list, nil
}

// UpdateCluster CAS-updates a cluster the caller owns; it never moves ownership.
func (c *Client) UpdateCluster(ctx context.Context, cluster *seaplanev1alpha1.Cluster) error {
	if err := validateOwnedSnapshot(ctx, cluster); err != nil {
		return err
	}
	return c.client.Update(ctx, cluster)
}

// DeleteCluster deletes a Cluster by name in the account namespace from ctx.
func (c *Client) DeleteCluster(ctx context.Context, name string) error {
	cluster, err := c.GetCluster(ctx, name)
	if err != nil {
		return err
	}
	return c.DeleteClusterObject(ctx, cluster)
}

// DeleteClusterObject deletes the checked snapshot without fetching newer state.
func (c *Client) DeleteClusterObject(ctx context.Context, cluster *seaplanev1alpha1.Cluster) error {
	if err := validateOwnedSnapshot(ctx, cluster); err != nil {
		return err
	}
	return c.client.Delete(ctx, cluster)
}

// ClaimSpotTerminationQueue atomically reserves a customer SQS queue URL for a
// Cluster. The bool reports whether this call created the reservation. An
// existing reservation owned by the same Cluster is treated as success.
func (c *Client) ClaimSpotTerminationQueue(ctx context.Context, queueURL, clusterName string) (bool, error) {
	accountID := middleware.GetAccountID(ctx)
	indexName := seaplanev1alpha1.SpotTerminationQueueIndexName(accountID, queueURL)
	ownerKey := spotQueueOwnerKey(clusterName)
	var lastConflict error

	for range 5 {
		reservation := &seaplanev1alpha1.Index{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: seaplanev1alpha1.SpotTerminationQueueReservationsNamespace,
				Name:      indexName,
				Labels: map[string]string{
					seaplanev1alpha1.SpotTerminationQueueOwnerLabel: ownerKey,
				},
				Annotations: map[string]string{
					seaplanev1alpha1.SpotTerminationQueueClaimedAtAnnotation: time.Now().UTC().Format(time.RFC3339Nano),
				},
			},
		}
		if err := c.client.Create(ctx, reservation); err == nil {
			return true, nil
		} else if !IsAlreadyExists(err) {
			return false, err
		} else {
			createErr := err
			lastConflict = createErr
			var existing seaplanev1alpha1.Index
			getErr := c.client.Get(ctx, k8stypes.NamespacedName{
				Namespace: seaplanev1alpha1.SpotTerminationQueueReservationsNamespace,
				Name:      indexName,
			}, &existing)
			if IsNotFound(getErr) {
				continue
			}
			if getErr != nil {
				return false, getErr
			}
			if existing.Labels[seaplanev1alpha1.SpotTerminationQueueOwnerLabel] == ownerKey {
				if existing.Annotations == nil {
					existing.Annotations = make(map[string]string)
				}
				existing.Annotations[seaplanev1alpha1.SpotTerminationQueueClaimedAtAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
				if err := c.client.Update(ctx, &existing); err != nil {
					if IsConflict(err) {
						continue
					}
					return false, err
				}
				return false, nil
			}

			ownerName := strings.TrimPrefix(existing.Labels[seaplanev1alpha1.SpotTerminationQueueOwnerLabel], spotQueueOwnerPrefix)
			if ownerName == existing.Labels[seaplanev1alpha1.SpotTerminationQueueOwnerLabel] || ownerName == "" {
				return false, createErr
			}
			ownerCluster, err := c.GetCluster(ctx, ownerName)
			if err == nil && clusterUsesSpotTerminationQueue(ownerCluster, queueURL) {
				return false, createErr
			}
			if err != nil && !IsNotFound(err) {
				return false, err
			}
			if !spotQueueClaimExpired(&existing) {
				return false, createErr
			}
			if err := c.client.Delete(ctx, &existing); err != nil && !IsNotFound(err) {
				if IsConflict(err) {
					continue
				}
				return false, err
			}
		}
	}

	if lastConflict != nil {
		return false, lastConflict
	}
	return false, fmt.Errorf("could not claim Spot termination queue after reclaiming stale reservations")
}

func spotQueueOwnerKey(clusterName string) string {
	return spotQueueOwnerPrefix + clusterName
}

func spotQueueClaimExpired(index *seaplanev1alpha1.Index) bool {
	claimedAt, err := time.Parse(time.RFC3339Nano, index.Annotations[seaplanev1alpha1.SpotTerminationQueueClaimedAtAnnotation])
	return err == nil && time.Since(claimedAt) > spotQueueClaimTimeout
}

func clusterUsesSpotTerminationQueue(cluster *seaplanev1alpha1.Cluster, queueURL string) bool {
	if cluster == nil || cluster.Spec.HostedCluster.Platform.AWS == nil {
		return false
	}
	return cluster.Spec.HostedCluster.Platform.AWS.TerminationHandlerQueueURL == queueURL
}

// ReleaseSpotTerminationQueue releases a reservation only when it is still
// owned by the given Cluster. A missing reservation is already released.
func (c *Client) ReleaseSpotTerminationQueue(ctx context.Context, queueURL, clusterName string) error {
	accountID := middleware.GetAccountID(ctx)
	var reservation seaplanev1alpha1.Index
	err := c.client.Get(ctx, k8stypes.NamespacedName{
		Namespace: seaplanev1alpha1.SpotTerminationQueueReservationsNamespace,
		Name:      seaplanev1alpha1.SpotTerminationQueueIndexName(accountID, queueURL),
	}, &reservation)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if reservation.Labels[seaplanev1alpha1.SpotTerminationQueueOwnerLabel] != spotQueueOwnerKey(clusterName) {
		return fmt.Errorf("spot termination queue reservation is not owned by cluster %q", clusterName)
	}
	if ownerCluster, err := c.GetCluster(ctx, clusterName); err == nil && clusterUsesSpotTerminationQueue(ownerCluster, queueURL) {
		return nil
	} else if err != nil && !IsNotFound(err) {
		return err
	}
	if err := c.client.Delete(ctx, &reservation); IsNotFound(err) {
		return nil
	} else {
		return err
	}
}

// --- NodePool operations ---

// CreateNodePool creates a NodePool under a parent owned by the account in ctx.
func (c *Client) CreateNodePool(ctx context.Context, np *seaplanev1alpha1.NodePool) error {
	accountID, err := validateOwnedCandidate(ctx, np, np.Spec.AccountID)
	if err != nil {
		return err
	}
	if err := c.requireOwnedCluster(ctx, np); err != nil {
		return err
	}
	np.Spec.AccountID = accountID
	return c.client.Create(ctx, np)
}

// requireOwnedCluster checks the stored parent name and UID, not child ownership labels.
func (c *Client) requireOwnedCluster(ctx context.Context, np *seaplanev1alpha1.NodePool) error {
	parent := nodePoolParent(np)
	if parent == nil {
		return apierrors.NewNotFound(nodePoolGR, np.Name)
	}
	cluster, err := c.GetCluster(ctx, parent.Name)
	if err != nil {
		return err
	}
	if cluster.UID != parent.UID {
		return apierrors.NewNotFound(nodePoolGR, np.Name)
	}
	return nil
}

func nodePoolParent(np *seaplanev1alpha1.NodePool) *metav1.OwnerReference {
	parent := metav1.GetControllerOf(np)
	if parent == nil || parent.Kind != "Cluster" || parent.UID == "" || np.Labels[clusterUIDLabel] != string(parent.UID) {
		return nil
	}
	return parent
}

// GetNodePool retrieves a NodePool by name in the account namespace from ctx.
// Callers check the parent binding; handlers do it while building the authz resource.
func (c *Client) GetNodePool(ctx context.Context, name string) (*seaplanev1alpha1.NodePool, error) {
	accountID := middleware.GetAccountID(ctx)
	var nodePool seaplanev1alpha1.NodePool
	err := c.client.Get(ctx, k8stypes.NamespacedName{
		Namespace: accountNamespace(accountID),
		Name:      name,
	}, &nodePool)
	if err != nil {
		return nil, err
	}
	return &nodePool, nil
}

// ListNodePools lists NodePools in the account namespace from ctx, optionally
// filtered by their parent Cluster UID label.
func (c *Client) ListNodePools(ctx context.Context, clusterUID string) (*seaplanev1alpha1.NodePoolList, error) {
	accountID := middleware.GetAccountID(ctx)
	var list seaplanev1alpha1.NodePoolList
	opts := []client.ListOption{client.InNamespace(accountNamespace(accountID))}
	if clusterUID != "" {
		opts = append(opts, client.MatchingLabels{clusterUIDLabel: clusterUID})
	}

	if err := c.client.List(ctx, &list, opts...); err != nil {
		return nil, err
	}
	return &list, nil
}

// UpdateNodePool CAS-updates a node pool the caller owns; it never moves ownership.
func (c *Client) UpdateNodePool(ctx context.Context, np *seaplanev1alpha1.NodePool) error {
	if err := validateOwnedSnapshot(ctx, np); err != nil {
		return err
	}
	return c.client.Update(ctx, np)
}

// DeleteNodePool deletes a NodePool by name in the account namespace from ctx.
func (c *Client) DeleteNodePool(ctx context.Context, name string) error {
	np, err := c.GetNodePool(ctx, name)
	if err != nil {
		return err
	}
	return c.DeleteNodePoolObject(ctx, np)
}

// DeleteNodePoolObject deletes the checked child snapshot, never an unchecked reload.
func (c *Client) DeleteNodePoolObject(ctx context.Context, np *seaplanev1alpha1.NodePool) error {
	if err := validateOwnedSnapshot(ctx, np); err != nil {
		return err
	}
	return c.client.Delete(ctx, np)
}

// --- DNSReservation operations ---

func (c *Client) CreateDNSReservation(ctx context.Context, reservation *seaplanev1alpha1.DNSReservation) error {
	if _, err := validateOwnedCandidate(ctx, reservation, ""); err != nil {
		return err
	}
	return c.client.Create(ctx, reservation)
}

func (c *Client) GetDNSReservation(ctx context.Context, name string) (*seaplanev1alpha1.DNSReservation, error) {
	accountID := middleware.GetAccountID(ctx)
	var reservation seaplanev1alpha1.DNSReservation
	err := c.client.Get(ctx, k8stypes.NamespacedName{
		Namespace: accountNamespace(accountID),
		Name:      name,
	}, &reservation)
	if err != nil {
		return nil, err
	}
	return &reservation, nil
}

// GetDNSReservationByUID resolves a reservation UID within an account namespace.
func (c *Client) GetDNSReservationByUID(ctx context.Context, uid string) (*seaplanev1alpha1.DNSReservation, error) {
	accountID := middleware.GetAccountID(ctx)
	var list seaplanev1alpha1.DNSReservationList
	if err := c.client.List(ctx, &list,
		client.InNamespace(accountNamespace(accountID)),
		client.MatchingFields{"metadata.uid": uid},
	); err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, apierrors.NewNotFound(dnsReservationGR, uid)
	}
	return &list.Items[0], nil
}

func (c *Client) ListDNSReservations(ctx context.Context) (*seaplanev1alpha1.DNSReservationList, error) {
	accountID := middleware.GetAccountID(ctx)
	var list seaplanev1alpha1.DNSReservationList
	if err := c.client.List(ctx, &list, client.InNamespace(accountNamespace(accountID))); err != nil {
		return nil, err
	}
	return &list, nil
}

func (c *Client) UpdateDNSReservation(ctx context.Context, reservation *seaplanev1alpha1.DNSReservation) error {
	if err := validateOwnedSnapshot(ctx, reservation); err != nil {
		return err
	}
	return c.client.Update(ctx, reservation)
}

func (c *Client) UpdateDNSReservationStatus(ctx context.Context, reservation *seaplanev1alpha1.DNSReservation) error {
	return c.client.Status().Update(ctx, reservation)
}

func (c *Client) DeleteDNSReservation(ctx context.Context, name string) error {
	reservation, err := c.GetDNSReservation(ctx, name)
	if err != nil {
		return err
	}
	return c.client.Delete(ctx, reservation)
}

func (c *Client) DeleteDNSReservationObject(ctx context.Context, reservation *seaplanev1alpha1.DNSReservation) error {
	return c.client.Delete(ctx, reservation)
}

// --- Manifest operations ---

// CreateManifest creates a Manifest resource in the given namespace.
func (c *Client) CreateManifest(ctx context.Context, namespace string, hfm *seaplanev1alpha1.Manifest) error {
	hfm.Namespace = namespace
	return c.client.Create(ctx, hfm)
}

// GetManifest retrieves a Manifest by namespace and name.
func (c *Client) GetManifest(ctx context.Context, namespace, name string) (*seaplanev1alpha1.Manifest, error) {
	var m seaplanev1alpha1.Manifest
	err := c.client.Get(ctx, k8stypes.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}, &m)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// DeleteManifest deletes a Manifest by namespace and name.
func (c *Client) DeleteManifest(ctx context.Context, namespace, name string) error {
	m := &seaplanev1alpha1.Manifest{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
	}
	return c.client.Delete(ctx, m)
}

// --- ManagementCluster operations ---

// CreateManagementCluster creates a ManagementCluster resource with global scope.
func (c *Client) CreateManagementCluster(ctx context.Context, mc *seaplanev1alpha1.ManagementCluster) error {
	mc.Namespace = ""
	return c.client.Create(ctx, mc)
}

// --- Account allowlist ---

// shortcut: one empty ConfigMap per enrolled account in seaplane-db; replace with DynamoDB account storage.
const enrolledAccountsNamespace = "accounts"

// EnrollAccount adds an account to the allowlist. Enrolling twice succeeds.
func (c *Client) EnrollAccount(ctx context.Context, accountID string) error {
	err := c.client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: enrolledAccountsNamespace, Name: accountID}})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// UnenrollAccount removes an account from the allowlist. Removing a missing account succeeds.
func (c *Client) UnenrollAccount(ctx context.Context, accountID string) error {
	err := c.client.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: enrolledAccountsNamespace, Name: accountID}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// IsAccountEnrolled reports whether an account is on the allowlist.
func (c *Client) IsAccountEnrolled(ctx context.Context, accountID string) (bool, error) {
	err := c.client.Get(ctx, client.ObjectKey{Namespace: enrolledAccountsNamespace, Name: accountID}, &corev1.ConfigMap{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// --- OidcConfig operations ---

// CreateOidcConfig creates an OidcConfig in the caller's account namespace.
func (c *Client) CreateOidcConfig(ctx context.Context, oc *seaplanev1alpha1.OidcConfig) error {
	if _, err := validateOwnedCandidate(ctx, oc, ""); err != nil {
		return err
	}
	return c.client.Create(ctx, oc)
}

// GetOidcConfig retrieves an OidcConfig by name for the account in ctx.
func (c *Client) GetOidcConfig(ctx context.Context, name string) (*seaplanev1alpha1.OidcConfig, error) {
	accountID := middleware.GetAccountID(ctx)
	var oc seaplanev1alpha1.OidcConfig
	err := c.client.Get(ctx, k8stypes.NamespacedName{
		Namespace: accountNamespace(accountID),
		Name:      name,
	}, &oc)
	if err != nil {
		return nil, err
	}
	return &oc, nil
}

// GetOidcConfigByUID resolves an OidcConfig UID within the account namespace from ctx.
func (c *Client) GetOidcConfigByUID(ctx context.Context, uid string) (*seaplanev1alpha1.OidcConfig, error) {
	accountID := middleware.GetAccountID(ctx)
	var list seaplanev1alpha1.OidcConfigList
	if err := c.client.List(ctx, &list,
		client.InNamespace(accountNamespace(accountID)),
		client.MatchingFields{"metadata.uid": uid},
	); err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, apierrors.NewNotFound(seaplanev1alpha1.GroupVersion.WithResource("oidcconfigs").GroupResource(), uid)
	}
	return &list.Items[0], nil
}

// ListOidcConfigs lists OidcConfigs for the account in ctx by namespace.
func (c *Client) ListOidcConfigs(ctx context.Context) (*seaplanev1alpha1.OidcConfigList, error) {
	accountID := middleware.GetAccountID(ctx)
	var list seaplanev1alpha1.OidcConfigList
	err := c.client.List(ctx, &list, client.InNamespace(accountNamespace(accountID)))
	if err != nil {
		return nil, err
	}
	return &list, nil
}

// DeleteOidcConfig deletes an OidcConfig by name for the account in ctx.
func (c *Client) DeleteOidcConfig(ctx context.Context, name string) error {
	oc, err := c.GetOidcConfig(ctx, name)
	if err != nil {
		return err
	}
	return c.DeleteOidcConfigObject(ctx, oc)
}

// UpdateOidcConfigLastUsedTimestamp sets status.lastUsedTimestamp on an
// OidcConfig, resolved by UID within the account in ctx.
func (c *Client) UpdateOidcConfigLastUsedTimestamp(ctx context.Context, oidcConfigUID string, ts metav1.Time) error {
	oc, err := c.GetOidcConfigByUID(ctx, oidcConfigUID)
	if err != nil {
		return err
	}
	oc.Status.LastUsedTimestamp = &ts
	return c.client.Status().Update(ctx, oc)
}

// UpdateOidcConfigObject updates oc (e.g. its labels) via CAS on its current ResourceVersion; oc must be freshly fetched (e.g. via GetOidcConfig) so a concurrent change surfaces as a conflict (IsConflict) instead of overwriting it.
func (c *Client) UpdateOidcConfigObject(ctx context.Context, oc *seaplanev1alpha1.OidcConfig) error {
	if err := validateOwnedSnapshot(ctx, oc); err != nil {
		return err
	}
	return c.client.Update(ctx, oc)
}

// DeleteOidcConfigObject deletes oc via CAS on its current ResourceVersion; oc must be freshly fetched (e.g. via GetOidcConfig) so a concurrent change surfaces as a conflict (IsConflict) instead of deleting stale state.
func (c *Client) DeleteOidcConfigObject(ctx context.Context, oc *seaplanev1alpha1.OidcConfig) error {
	if err := validateOwnedSnapshot(ctx, oc); err != nil {
		return err
	}
	return c.client.Delete(ctx, oc)
}

// validateOwnedCandidate rejects a create whose namespace, account label, or spec
// account differs from the caller's, so what is written matches what was authorized.
// An empty spec account is allowed; the caller then sets it.
func validateOwnedCandidate(ctx context.Context, obj client.Object, specAccountID string) (string, error) {
	account := middleware.GetAccountID(ctx)
	if account == "" || obj.GetNamespace() != accountNamespace(account) || obj.GetLabels()[accountIDLabel] != account || (specAccountID != "" && specAccountID != account) {
		return "", fmt.Errorf("create candidate identity does not match caller")
	}
	return account, nil
}

func validateOwnedSnapshot(ctx context.Context, obj client.Object) error {
	if err := seaplanedb.ValidateObjectResourceVersion(obj); err != nil {
		return err
	}
	account := middleware.GetAccountID(ctx)
	if account == "" || obj.GetNamespace() != accountNamespace(account) || obj.GetName() == "" {
		return fmt.Errorf("invalid snapshot identity")
	}
	if owner, exists := obj.GetLabels()[accountIDLabel]; exists && owner != account {
		return fmt.Errorf("snapshot owner does not match caller")
	}
	return nil
}

// GetOidcIssuerIndex is a best-effort, non-atomic fast-path check for whether indexName is reserved.
func (c *Client) GetOidcIssuerIndex(ctx context.Context, indexName string) (*seaplanev1alpha1.Index, error) {
	var idx seaplanev1alpha1.Index
	err := c.client.Get(ctx, k8stypes.NamespacedName{
		Namespace: seaplanev1alpha1.OidcIssuerReservationsNamespace,
		Name:      indexName,
	}, &idx)
	if err != nil {
		return nil, err
	}
	return &idx, nil
}

// --- Error helpers ---

// IsNotFound returns true if the error is a Kubernetes 404.
func IsNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

// IsAlreadyExists returns true if the error is a Kubernetes 409 (already exists).
func IsAlreadyExists(err error) bool {
	return apierrors.IsAlreadyExists(err)
}

// IsConflict returns true if the error is a Kubernetes 409 from a CAS Update/Delete, distinct from IsAlreadyExists which is a 409 from a colliding Create.
func IsConflict(err error) bool {
	return apierrors.IsConflict(err)
}

// --- internal helpers ---

var (
	nodePoolGR       = seaplanev1alpha1.GroupVersion.WithResource("nodepools").GroupResource()
	dnsReservationGR = seaplanev1alpha1.GroupVersion.WithResource("dnsreservations").GroupResource()
)

const accountNSPrefix = "account-"

func accountNamespace(accountID string) string {
	return accountNSPrefix + accountID
}
