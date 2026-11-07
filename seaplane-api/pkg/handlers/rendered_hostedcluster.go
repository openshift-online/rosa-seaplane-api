package handlers

import (
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/api"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

// RenderedHostedClusterHandler serves the read-only rendered HostedCluster API.
type RenderedHostedClusterHandler struct {
	db         *seaplanedb.Client
	logger     *slog.Logger
	authorizer *authz.Authorizer
}

func NewRenderedHostedClusterHandler(db *seaplanedb.Client, authorizer *authz.Authorizer, logger *slog.Logger) *RenderedHostedClusterHandler {
	if authorizer == nil {
		panic("rendered HostedCluster authorizer is required")
	}
	return &RenderedHostedClusterHandler{db: db, authorizer: authorizer, logger: logger}
}

// List handles GET /api/v0/renderedhostedclusters.
func (h *RenderedHostedClusterHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountID := middleware.GetAccountID(ctx)
	prepared, attempt := authorizeCollection(w, r, h.authorizer, h.logger, authz.ListClusters, authz.Resource{
		Kind: authz.Collection, CollectionKind: authz.Cluster, AccountID: accountID,
	})
	if prepared == nil {
		return
	}

	list, err := h.db.ListRenderedHostedClusters(ctx)
	if err != nil {
		h.logger.Error("failed to list rendered HostedClusters", "error_type", "backend")
		h.logger.Debug("rendered HostedCluster list backend error", "error", err)
		attempt.Finish(authz.OutcomeError, authz.StageResourceLoading)
		writeAPIError(w, ErrRenderedHostedClusterList, h.logger)
		return
	}
	attempt.Finish(authz.OutcomeAllow, authz.StageNone)
	writeRenderedList(w, r, list.Items, func(item hypershiftv1beta1.HostedCluster) string { return item.Name }, h.logger)
}

// Get handles GET /api/v0/renderedhostedclusters/{id}.
func (h *RenderedHostedClusterHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	clusterName := mux.Vars(r)["id"]
	rendered, err := h.db.GetRenderedHostedCluster(ctx, clusterName)
	if err != nil {
		if seaplanedb.IsNotFound(err) {
			writeAPIError(w, ErrRenderedHostedClusterGetNotFound, h.logger)
		} else {
			writeAPIError(w, ErrRenderedHostedClusterGetFailed, h.logger)
		}
		return
	}
	cluster, err := h.db.GetCluster(ctx, clusterName)
	if err != nil {
		if seaplanedb.IsNotFound(err) {
			writeAPIError(w, ErrRenderedHostedClusterGetNotFound, h.logger)
		} else {
			writeAPIError(w, ErrRenderedHostedClusterGetFailed, h.logger)
		}
		return
	}
	if !authorizeResource(w, r, h.authorizer, h.logger, authz.DescribeCluster, clusterAuthzResource(cluster)) {
		return
	}
	if err := api.Write(w, http.StatusOK, rendered); err != nil {
		h.logger.Error("failed to write rendered HostedCluster response", "error_type", "response")
		h.logger.Debug("rendered HostedCluster response write error", "error", err)
	}
}

// writeRenderedList writes a stable, account-scoped rendered-resource page.
// The database list order is not part of the API contract, so sort before
// applying offset/limit pagination.
func writeRenderedList[T any](w http.ResponseWriter, r *http.Request, items []T, name func(T) string, logger *slog.Logger) {
	limit := 50
	offset := 0
	if value, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && value > 0 && value <= 100 {
		limit = value
	}
	if value, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && value >= 0 {
		offset = value
	}
	sort.SliceStable(items, func(i, j int) bool { return name(items[i]) < name(items[j]) })
	total := len(items)
	if offset >= total {
		items = []T{}
	} else {
		items = items[offset:min(offset+limit, total)]
	}
	if err := api.Write(w, http.StatusOK, map[string]any{"items": items, "total": total, "limit": limit, "offset": offset}); err != nil {
		logger.Error("failed to write rendered resource list response", "error_type", "response")
		logger.Debug("rendered resource list response write error", "error", err)
	}
}
