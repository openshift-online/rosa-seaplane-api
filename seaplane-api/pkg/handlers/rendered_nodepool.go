package handlers

import (
	"log/slog"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/api"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

// RenderedNodePoolHandler serves the read-only rendered NodePool API.
type RenderedNodePoolHandler struct {
	db         *seaplanedb.Client
	logger     *slog.Logger
	authorizer *authz.Authorizer
}

func NewRenderedNodePoolHandler(db *seaplanedb.Client, authorizer *authz.Authorizer, logger *slog.Logger) *RenderedNodePoolHandler {
	if authorizer == nil {
		panic("rendered NodePool authorizer is required")
	}
	return &RenderedNodePoolHandler{db: db, authorizer: authorizer, logger: logger}
}

// List handles GET /api/v0/renderednodepools.
func (h *RenderedNodePoolHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountID := middleware.GetAccountID(ctx)
	prepared, attempt := authorizeCollection(w, r, h.authorizer, h.logger, authz.ListNodePools, authz.Resource{
		Kind: authz.Collection, CollectionKind: authz.NodePool, AccountID: accountID,
	})
	if prepared == nil {
		return
	}

	list, err := h.db.ListRenderedNodePools(ctx)
	if err != nil {
		h.logger.Error("failed to list rendered NodePools", "error_type", "backend")
		h.logger.Debug("rendered NodePool list backend error", "error", err)
		attempt.Finish(authz.OutcomeError, authz.StageResourceLoading)
		writeAPIError(w, ErrRenderedNodePoolList, h.logger)
		return
	}
	attempt.Finish(authz.OutcomeAllow, authz.StageNone)
	writeRenderedList(w, r, list.Items, func(item hypershiftv1beta1.NodePool) string { return item.Name }, h.logger)
}

// Get handles GET /api/v0/renderednodepools/{id}.
func (h *RenderedNodePoolHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nodepoolName := mux.Vars(r)["id"]
	rendered, err := h.db.GetRenderedNodePool(ctx, nodepoolName)
	if err != nil {
		if seaplanedb.IsNotFound(err) {
			writeAPIError(w, ErrRenderedNodePoolGetNotFound, h.logger)
		} else {
			writeAPIError(w, ErrRenderedNodePoolGetFailed, h.logger)
		}
		return
	}
	source, err := h.db.GetNodePool(ctx, nodepoolName)
	if seaplanedb.IsNotFound(err) {
		writeAPIError(w, ErrRenderedNodePoolGetNotFound, h.logger)
		return
	}
	if err != nil {
		writeAPIError(w, ErrRenderedNodePoolGetFailed, h.logger)
		return
	}
	resource, authzErr := nodePoolAuthzResource(ctx, h.db, source)
	if authzErr != nil {
		if seaplanedb.IsNotFound(authzErr) {
			writeAPIError(w, ErrRenderedNodePoolGetNotFound, h.logger)
			return
		}
		attempt := startResourceAuthz(r, authz.DescribeNodePool)
		writeResourceAuthzError(w, r, attempt, authz.DescribeNodePool, authzErr, h.logger)
		return
	}
	if !authorizeResource(w, r, h.authorizer, h.logger, authz.DescribeNodePool, resource) {
		return
	}
	if err := api.Write(w, http.StatusOK, rendered); err != nil {
		h.logger.Error("failed to write rendered NodePool response", "error_type", "response")
		h.logger.Debug("rendered NodePool response write error", "error", err)
	}
}
