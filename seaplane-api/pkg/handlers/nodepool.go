package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/util/retry"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	public "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/internal/codegen/featuregate"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/api"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/validation"
)

type NodePoolHandler struct {
	db         *seaplanedb.Client
	validator  *validation.FieldValidator
	logger     *slog.Logger
	authorizer *authz.Authorizer
}

func NewNodePoolHandler(db *seaplanedb.Client, authorizer *authz.Authorizer, logger *slog.Logger) *NodePoolHandler {
	if authorizer == nil {
		panic("node pool authorizer is required")
	}
	return &NodePoolHandler{
		authorizer: authorizer,
		db:         db,
		validator:  validation.NewFieldValidator("NodePool"),
		logger:     logger,
	}
}

func validateNodePoolReplicas(spec *public.NodePoolSpec) validation.ValidationErrors {
	if spec == nil || spec.NodePool.Replicas == nil || *spec.NodePool.Replicas >= 0 {
		return nil
	}
	return validation.ValidationErrors{
		&validation.ValidationError{
			Field:  "spec.nodePool.replicas",
			Reason: "must be greater than or equal to 0",
		},
	}
}

func validateNodePoolEC2InstanceMetadataHttpTokens(value public.EC2InstanceMetadataHttpTokens) validation.ValidationErrors {
	if slices.Contains([]public.EC2InstanceMetadataHttpTokens{
		public.EC2InstanceMetadataHttpTokensRequired,
		public.EC2InstanceMetadataHttpTokensOptional,
	}, value) {
		return nil
	}
	return validation.ValidationErrors{
		&validation.ValidationError{
			Field:  "spec.ec2InstanceMetadataHttpTokens",
			Reason: "must be either required or optional",
		},
	}
}

func (h *NodePoolHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountID := middleware.GetAccountID(ctx)
	clusterUID, apiErr := listClusterUID(r.URL.Query())
	if apiErr != nil {
		writeAPIError(w, *apiErr, h.logger)
		return
	}
	prepared, attempt := authorizeCollection(w, r, h.authorizer, h.logger, authz.ListNodePools, authz.Resource{Kind: authz.Collection, CollectionKind: authz.NodePool, AccountID: accountID})
	if prepared == nil {
		return
	}

	limitStr := r.URL.Query().Get("limit")
	offsetStr := r.URL.Query().Get("offset")

	limit := 50
	offset := 0

	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 100 {
			limit = l
		}
	}

	if offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
			offset = o
		}
	}

	h.logger.Info("listing nodepools", "account_id", accountID, "limit", limit, "offset", offset, "cluster_uid", clusterUID)

	list, err := h.db.ListNodePools(ctx, clusterUID)
	if err != nil {
		h.logger.Error("failed to list nodepools", "error", err, "account_id", accountID)
		attempt.Finish(authz.OutcomeError, authz.StageResourceLoading)
		writeAPIError(w, ErrNodePoolList, h.logger)
		return
	}

	// List permission returns every item in full; Describe is not checked per item.
	nodepools := make([]*public.NodePool, 0, len(list.Items))
	for i := range list.Items {
		nodepools = append(nodepools, seaplanedb.InternalToPublicNodePool(&list.Items[i]))
	}
	attempt.Finish(authz.OutcomeAllow, authz.StageNone)

	total := len(nodepools)

	if offset >= len(nodepools) {
		nodepools = nil
	} else {
		end := offset + min(limit, len(nodepools)-offset)
		nodepools = nodepools[offset:end]
	}

	response := map[string]any{
		"items":  nodepools,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	}

	if err := api.Write(w, http.StatusOK, response); err != nil {
		h.logger.Error("failed to write response", "error", err)
	}
}

// listClusterUID resolves the optional parent Cluster UID filter from clusterUID,
// clusterId, or a cluster-uid labelSelector.
func listClusterUID(query url.Values) (string, *APIError) {
	clusterUID := query.Get("clusterUID")
	clusterID := query.Get("clusterId")
	if clusterUID != "" && clusterID != "" && clusterUID != clusterID {
		return "", &ErrNodePoolListInvalidClusterUID
	}
	if clusterUID == "" {
		// The existing OpenAPI list parameter is named clusterId. Its value is
		// now the parent Cluster UID and is applied as the cluster-uid label.
		clusterUID = clusterID
	}
	if selector := query.Get("labelSelector"); selector != "" {
		parsed, err := labels.Parse(selector)
		if err != nil {
			return "", &ErrNodePoolListInvalidLabelSelector
		}
		requirements, selectable := parsed.Requirements()
		if !selectable || len(requirements) != 1 || requirements[0].Key() != clusterUIDLabel ||
			(requirements[0].Operator() != selection.Equals && requirements[0].Operator() != selection.DoubleEquals) ||
			requirements[0].Values().Len() != 1 {
			return "", &ErrNodePoolListInvalidLabelSelector
		}
		selectedUID := requirements[0].Values().List()[0]
		if clusterUID != "" && clusterUID != selectedUID {
			return "", &ErrNodePoolListInvalidLabelSelector
		}
		clusterUID = selectedUID
	}
	if clusterUID != "" {
		parsedUID, err := uuid.Parse(clusterUID)
		if err != nil || parsedUID.String() != clusterUID {
			return "", &ErrNodePoolListInvalidClusterUID
		}
	}
	return clusterUID, nil
}

// Create handles POST /api/v0/nodepools
// Request body: public.NodePool (K8s-native). Name is <cluster>.<child>; the
// prefix resolves the parent Cluster in the authenticated account namespace.
func (h *NodePoolHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountID := middleware.GetAccountID(ctx)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, ErrNodePoolCreateInvalidBody, h.logger)
		return
	}
	var req public.NodePool
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		writeAPIError(w, ErrNodePoolCreateInvalidBody, h.logger)
		return
	}
	var rawRequest map[string]any
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&rawRequest); err != nil {
		writeAPIError(w, ErrNodePoolCreateInvalidBody, h.logger)
		return
	}

	if req.Name == "" {
		writeAPIError(w, ErrNodePoolCreateMissingFields, h.logger)
		return
	}
	if err := validation.ValidateAccountNamespace(req.Namespace, accountID); err != nil {
		writeAPIError(w, ErrNodePoolCreateInvalidNamespace, h.logger)
		return
	}
	clusterName, _, err := validation.ParseNodePoolName(req.Name)
	if err != nil {
		writeAPIError(w, ErrNodePoolCreateInvalidName, h.logger)
		return
	}
	// ClusterName is derived from the validated metadata.name prefix; do not
	// trust a separately supplied passthrough value to select a parent.
	req.Spec.NodePool.ClusterName = clusterName
	// Apply defaults only to omitted fields; explicit empty or null values are
	// retained for enum validation below.
	if !validation.HasJSONField(rawRequest, "spec.ec2InstanceMetadataHttpTokens") {
		req.Spec.EC2InstanceMetadataHttpTokens = public.EC2InstanceMetadataHttpTokensRequired
	}

	errs := append(h.validator.ValidateCreate(&req.Spec, featuregate.Default), validateNodePoolReplicas(&req.Spec)...)
	errs = append(errs, validation.ValidateCustomerAWSTags(req.Spec.Tags, "spec.tags")...)
	errs = append(errs, validateNodePoolEC2InstanceMetadataHttpTokens(req.Spec.EC2InstanceMetadataHttpTokens)...)
	if len(errs) > 0 {
		writeAPIError(w, ErrNodePoolValidation.WithErrors(errs), h.logger)
		return
	}

	cluster, err := h.db.GetCluster(ctx, clusterName)
	if err != nil {
		if seaplanedb.IsNotFound(err) {
			writeAPIError(w, ErrNodePoolCreateClusterNotFound, h.logger)
			return
		}
		h.logger.Error("failed to verify cluster exists", "error", err, "account_id", accountID, "cluster_name", clusterName)
		writeAPIError(w, ErrNodePoolCreateClusterCheck, h.logger)
		return
	}
	if !cluster.DeletionTimestamp.IsZero() {
		writeAPIError(w, ErrNodePoolCreateClusterDeleting, h.logger)
		return
	}
	if errs := validation.ValidateNodePoolAWSTagBudget(cluster.Spec.Tags, req.Spec.Tags, "spec.tags"); len(errs) > 0 {
		writeAPIError(w, ErrNodePoolValidation.WithErrors(errs), h.logger)
		return
	}
	h.logger.Info("creating nodepool", "account_id", accountID, "cluster_uid", cluster.UID, "nodepool_name", req.Name)

	// internalPoolID is retained as a service-set implementation field; FleetDB UID
	// is the public object identity.
	internalPoolID := uuid.New().String()
	if !validateCreateLabels(w, req.Annotations, h.logger) {
		return
	}
	cr := seaplanedb.PublicToInternalNodePool(&req, accountID, internalPoolID)
	cr.Spec.NodePool.ClusterName = cluster.Name
	cr.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(cluster, seaplanev1alpha1.GroupVersion.WithKind("Cluster"))}
	cr.Labels[clusterUIDLabel] = string(cluster.UID)

	if !h.authorizeNodePool(w, withRequestLabels(r, cr.Annotations), authz.CreateNodePool, cr) {
		return
	}
	if err := h.db.CreateNodePool(ctx, cr); err != nil {
		h.logger.Error("failed to create nodepool", "error", err, "account_id", accountID)
		if seaplanedb.IsAlreadyExists(err) {
			writeAPIError(w, ErrNodePoolCreateNameConflict, h.logger)
			return
		}
		writeAPIError(w, ErrNodePoolCreateFailed, h.logger)
		return
	}

	if err := api.Write(w, http.StatusCreated, seaplanedb.InternalToPublicNodePool(cr)); err != nil {
		h.logger.Error("failed to write response", "error", err)
	}
}

func (h *NodePoolHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountID := middleware.GetAccountID(ctx)
	vars := mux.Vars(r)
	nodepoolName := vars["id"]

	h.logger.Info("getting nodepool", "account_id", accountID, "nodepool_name", nodepoolName)

	cr, err := h.db.GetNodePool(ctx, nodepoolName)
	if err != nil {
		if seaplanedb.IsNotFound(err) {
			writeAPIError(w, ErrNodePoolGetNotFound, h.logger)
			return
		}
		h.logger.Error("failed to get nodepool", "error", err, "account_id", accountID, "nodepool_name", nodepoolName)
		writeAPIError(w, ErrNodePoolGetFailed, h.logger)
		return
	}

	if !h.authorizeNodePool(w, r, authz.DescribeNodePool, cr) {
		return
	}
	if err := api.Write(w, http.StatusOK, seaplanedb.InternalToPublicNodePool(cr)); err != nil {
		h.logger.Error("failed to write response", "error", err)
	}
}

// Update handles PUT /api/v0/nodepools/{id}
// Request body: public.NodePool (K8s-native). Only spec fields are merged; metadata is ignored.
func (h *NodePoolHandler) Update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountID := middleware.GetAccountID(ctx)
	vars := mux.Vars(r)
	nodepoolName := vars["id"]

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, ErrNodePoolUpdateInvalidBody, h.logger)
		return
	}

	var req public.NodePool
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPIError(w, ErrNodePoolUpdateInvalidBody, h.logger)
		return
	}
	if err := validation.ValidateAccountNamespace(req.Namespace, accountID); err != nil {
		writeAPIError(w, ErrNodePoolUpdateInvalidNamespace, h.logger)
		return
	}

	h.logger.Info("updating nodepool", "account_id", accountID, "nodepool_name", nodepoolName)

	var envelope struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeAPIError(w, ErrNodePoolUpdateInvalidBody, h.logger)
		return
	}

	var rawSpec map[string]any
	if err := json.Unmarshal(envelope.Spec, &rawSpec); err != nil {
		writeAPIError(w, ErrNodePoolUpdateInvalidBody, h.logger)
		return
	}
	if len(rawSpec) == 0 {
		writeAPIError(w, ErrNodePoolUpdateMissingFields, h.logger)
		return
	}

	// Retry conflicts like upstream. Each attempt re-reads, re-validates, and
	// re-authorizes, so no write uses a snapshot that was not checked.
	r, finish := withAuthzOutcome(r)
	defer finish()
	var updated *seaplanev1alpha1.NodePool
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var attemptErr error
		updated, attemptErr = h.updateOnce(w, r, nodepoolName, &req.Spec, rawSpec, envelope.Spec)
		return attemptErr
	})
	if errors.Is(err, errResponded) {
		return
	}
	if err != nil {
		writeAPIError(w, ErrNodePoolUpdateConflict, h.logger)
		return
	}

	if err := api.Write(w, http.StatusOK, seaplanedb.InternalToPublicNodePool(updated)); err != nil {
		h.logger.Error("failed to write response", "error", err)
	}
}

// updateOnce makes one checked update attempt; see ClusterHandler.updateOnce.
func (h *NodePoolHandler) updateOnce(w http.ResponseWriter, r *http.Request, nodepoolName string, reqSpec *public.NodePoolSpec, rawSpec map[string]any, spec json.RawMessage) (*seaplanev1alpha1.NodePool, error) {
	ctx := r.Context()
	accountID := middleware.GetAccountID(ctx)
	cr, err := h.db.GetNodePool(ctx, nodepoolName)
	if err != nil {
		if seaplanedb.IsNotFound(err) {
			writeAPIError(w, ErrNodePoolUpdateNotFound, h.logger)
			return nil, errResponded
		}
		h.logger.Error("failed to get nodepool for update", "error", err, "account_id", accountID, "nodepool_name", nodepoolName)
		writeAPIError(w, ErrNodePoolUpdateFailed, h.logger)
		return nil, errResponded
	}
	// Authorize before validation, so a caller without UpdateNodePool learns no field details.
	if !h.authorizeNodePool(w, r, authz.UpdateNodePool, cr) {
		return nil, errResponded
	}

	errs := h.validator.ValidateUpdate(rawSpec, &cr.Spec, featuregate.Default)
	errs = append(errs, validateNodePoolReplicas(reqSpec)...)
	if len(errs) > 0 {
		writeAPIError(w, ErrNodePoolValidation.WithErrors(errs), h.logger)
		return nil, errResponded
	}
	candidate := cr.DeepCopy()
	if err := seaplanedb.MergeSpecJSON(&candidate.Spec, spec); err != nil {
		h.logger.Error("failed to merge nodepool spec", "error", err)
		writeAPIError(w, ErrNodePoolUpdateInvalidSpec, h.logger)
		return nil, errResponded
	}
	if err := h.validator.PreserveServiceSet(&candidate.Spec, &cr.Spec, featuregate.Default); err != nil {
		h.logger.Error("failed to preserve service-set fields", "error", err, "account_id", accountID, "nodepool_name", nodepoolName)
		writeAPIError(w, ErrNodePoolUpdateFailed, h.logger)
		return nil, errResponded
	}

	// Authorize the stored snapshot and every changed-field action before its CAS write.
	if !h.authorizeNodePool(w, r, authz.UpdateNodePool, cr, nodePoolUpdateActions(&cr.Spec, &candidate.Spec)...) {
		return nil, errResponded
	}
	if nodePoolTagsSupplied(rawSpec) {
		errs, err := h.validateUpdatedNodePoolTags(ctx, candidate)
		if len(errs) > 0 {
			writeAPIError(w, ErrNodePoolValidation.WithErrors(errs), h.logger)
			return nil, errResponded
		}
		if err != nil {
			h.logger.Error("failed to validate nodepool AWS tags", "error", err, "account_id", accountID, "nodepool_name", nodepoolName)
			writeAPIError(w, ErrNodePoolUpdateFailed, h.logger)
			return nil, errResponded
		}
	}
	if err := h.db.UpdateNodePool(ctx, candidate); err != nil {
		if seaplanedb.IsConflict(err) {
			return nil, err
		}
		if seaplanedb.IsNotFound(err) {
			writeAPIError(w, ErrNodePoolUpdateNotFound, h.logger)
			return nil, errResponded
		}
		h.logger.Error("failed to update nodepool", "error", err, "account_id", accountID, "nodepool_name", nodepoolName)
		writeAPIError(w, ErrNodePoolUpdateFailed, h.logger)
		return nil, errResponded
	}
	return candidate, nil
}

// validateUpdatedNodePoolTags checks the pool's AWS tags and its budget under the parent's tags.
func (h *NodePoolHandler) validateUpdatedNodePoolTags(ctx context.Context, np *seaplanev1alpha1.NodePool) (validation.ValidationErrors, error) {
	if errs := validation.ValidateCustomerAWSTags(np.Spec.Tags, "spec.tags"); len(errs) > 0 {
		return errs, nil
	}
	clusterName := np.Spec.NodePool.ClusterName
	if clusterName == "" {
		var err error
		if clusterName, _, err = validation.ParseNodePoolName(np.Name); err != nil {
			return validation.ValidationErrors{&validation.ValidationError{Field: "spec.tags", Reason: "unable to resolve the parent Cluster for tag budget validation"}}, nil
		}
	}
	cluster, err := h.db.GetCluster(ctx, clusterName)
	if seaplanedb.IsNotFound(err) {
		return validation.ValidationErrors{&validation.ValidationError{Field: "spec.tags", Reason: "parent Cluster must be available for tag budget validation"}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get parent Cluster for tag budget validation: %w", err)
	}
	return validation.ValidateNodePoolAWSTagBudget(cluster.Spec.Tags, np.Spec.Tags, "spec.tags"), nil
}

func nodePoolTagsSupplied(rawSpec map[string]any) bool {
	_, exists := rawSpec["tags"]
	return exists
}

func (h *NodePoolHandler) Delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountID := middleware.GetAccountID(ctx)
	vars := mux.Vars(r)
	nodepoolName := vars["id"]

	h.logger.Info("deleting nodepool", "account_id", accountID, "nodepool_name", nodepoolName)

	cr, err := h.db.GetNodePool(ctx, nodepoolName)
	if err != nil {
		if seaplanedb.IsNotFound(err) {
			writeAPIError(w, ErrNodePoolDeleteNotFound, h.logger)
			return
		}
		h.logger.Error("failed to get nodepool for delete", "error", err, "account_id", accountID, "nodepool_name", nodepoolName)
		writeAPIError(w, ErrNodePoolDeleteFailed, h.logger)
		return
	}
	if !h.authorizeNodePool(w, r, authz.DeleteNodePool, cr) {
		return
	}
	err = h.db.DeleteNodePoolObject(ctx, cr)
	if err != nil {
		if seaplanedb.IsConflict(err) {
			writeAPIError(w, ErrResourceConflict, h.logger)
			return
		}
		if seaplanedb.IsNotFound(err) {
			writeAPIError(w, ErrNodePoolDeleteNotFound, h.logger)
			return
		}
		h.logger.Error("failed to delete nodepool", "error", err, "account_id", accountID, "nodepool_name", nodepoolName)
		writeAPIError(w, ErrNodePoolDeleteFailed, h.logger)
		return
	}

	response := map[string]any{
		"message":       "NodePool deletion initiated",
		"nodepool_name": nodepoolName,
	}

	if err := api.Write(w, http.StatusAccepted, response); err != nil {
		h.logger.Error("failed to write response", "error", err)
	}
}
