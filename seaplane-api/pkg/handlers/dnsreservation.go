package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	"k8s.io/client-go/util/retry"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	public "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/api"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/validation"
)

var errDNSReservationClaimed = errors.New("DNS reservation is claimed")

// DNSReservationHandler serves the customer-facing DNS reservation API.
type DNSReservationHandler struct {
	db         *seaplanedb.Client
	authorizer *authz.Authorizer
	logger     *slog.Logger
}

func NewDNSReservationHandler(db *seaplanedb.Client, authorizer *authz.Authorizer, logger *slog.Logger) *DNSReservationHandler {
	if authorizer == nil {
		panic("DNS reservation authorizer is required")
	}
	return &DNSReservationHandler{db: db, authorizer: authorizer, logger: logger}
}

func (h *DNSReservationHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountID := middleware.GetAccountID(ctx)
	prepared, attempt := authorizeCollection(w, r, h.authorizer, h.logger, authz.ListDNSReservations, authz.Resource{Kind: authz.Collection, CollectionKind: authz.DNSReservation, AccountID: accountID})
	if prepared == nil {
		return
	}
	limit := parsePositiveInt(r.URL.Query().Get("limit"), 50, 100)
	offset := parseNonNegativeInt(r.URL.Query().Get("offset"))

	list, err := h.db.ListDNSReservations(ctx)
	if err != nil {
		h.logger.Error("failed to list DNS reservations", "error", err, "account_id", accountID)
		attempt.Finish(authz.OutcomeError, authz.StageResourceLoading)
		writeAPIError(w, ErrDNSReservationListFailed, h.logger)
		return
	}

	// List permission returns every item in full; Describe is not checked per item.
	items := make([]*public.DNSReservation, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, seaplanedb.InternalToPublicDNSReservation(&list.Items[i]))
	}
	attempt.Finish(authz.OutcomeAllow, authz.StageNone)
	total := len(items)
	if offset >= len(items) {
		items = []*public.DNSReservation{}
	} else {
		end := min(offset+limit, len(items))
		items = items[offset:end]
	}

	if err := api.Write(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	}); err != nil {
		h.logger.Error("failed to write DNS reservation list", "error", err)
	}
}

func (h *DNSReservationHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountID := middleware.GetAccountID(ctx)
	var req public.DNSReservation
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, ErrDNSReservationCreateInvalidBody, h.logger)
		return
	}
	if req.Name == "" {
		writeAPIError(w, ErrDNSReservationCreateMissingName, h.logger)
		return
	}
	if err := validation.ValidateResourceName(req.Name); err != nil {
		writeAPIError(w, ErrDNSReservationCreateInvalidName, h.logger)
		return
	}
	if err := validation.ValidateAccountNamespace(req.Namespace, accountID); err != nil {
		writeAPIError(w, ErrDNSReservationCreateNamespaceMismatch, h.logger)
		return
	}

	if !validateCreateLabels(w, req.Annotations, h.logger) {
		return
	}
	reservation := seaplanedb.PublicToInternalDNSReservation(&req, accountID)
	if !authorizeResource(w, withRequestLabels(r, reservation.Annotations), h.authorizer, h.logger, authz.CreateDNSReservation, candidateResource(dnsReservationResource(reservation), reservation.Name)) {
		return
	}
	reservation.Status.Phase = "Pending"
	if err := h.db.CreateDNSReservation(ctx, reservation); err != nil {
		if seaplanedb.IsAlreadyExists(err) {
			writeAPIError(w, ErrDNSReservationCreateNameConflict, h.logger)
			return
		}
		h.logger.Error("failed to create DNS reservation", "error", err, "account_id", accountID)
		writeAPIError(w, ErrDNSReservationCreateFailed, h.logger)
		return
	}
	if err := api.Write(w, http.StatusCreated, seaplanedb.InternalToPublicDNSReservation(reservation)); err != nil {
		h.logger.Error("failed to write DNS reservation response", "error", err)
	}
}

func (h *DNSReservationHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountID := middleware.GetAccountID(ctx)
	name := mux.Vars(r)["id"]
	reservation, err := h.db.GetDNSReservation(ctx, name)
	if err != nil {
		if seaplanedb.IsNotFound(err) {
			writeAPIError(w, ErrDNSReservationGetNotFound, h.logger)
			return
		}
		h.logger.Error("failed to get DNS reservation", "error", err, "account_id", accountID, "name", name)
		writeAPIError(w, ErrDNSReservationGetFailed, h.logger)
		return
	}
	if !authorizeResource(w, r, h.authorizer, h.logger, authz.DescribeDNSReservation, dnsReservationResource(reservation)) {
		return
	}
	if err := api.Write(w, http.StatusOK, seaplanedb.InternalToPublicDNSReservation(reservation)); err != nil {
		h.logger.Error("failed to write DNS reservation response", "error", err)
	}
}

func (h *DNSReservationHandler) Delete(w http.ResponseWriter, r *http.Request) {
	r, finish := withAuthzOutcome(r)
	defer finish()
	ctx := r.Context()
	accountID := middleware.GetAccountID(ctx)
	name := mux.Vars(r)["id"]
	var notFound bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		reservation, err := h.db.GetDNSReservation(ctx, name)
		if err != nil {
			if seaplanedb.IsNotFound(err) {
				notFound = true
			}
			return err
		}
		// Authorize each attempt's stored snapshot before revealing its claim state.
		if !authorizeResource(w, r, h.authorizer, h.logger, authz.DeleteDNSReservation, dnsReservationResource(reservation)) {
			return errResponded
		}
		if reservation.Labels[seaplanev1alpha1.ClaimedByClusterUIDLabel] != "" {
			return errDNSReservationClaimed
		}
		return h.db.DeleteDNSReservationObject(ctx, reservation)
	})
	if errors.Is(err, errResponded) {
		return
	}
	if notFound || seaplanedb.IsNotFound(err) {
		writeAPIError(w, ErrDNSReservationDeleteNotFound, h.logger)
		return
	}
	if errors.Is(err, errDNSReservationClaimed) {
		writeAPIError(w, ErrDNSReservationDeleteClaimed, h.logger)
		return
	}
	if err != nil {
		h.logger.Error("failed to delete DNS reservation", "error", err, "account_id", accountID, "name", name)
		writeAPIError(w, ErrDNSReservationDeleteFailed, h.logger)
		return
	}
	if err := api.Write(w, http.StatusAccepted, map[string]any{
		"message": "DNS reservation deletion initiated",
		"name":    name,
	}); err != nil {
		h.logger.Error("failed to write DNS reservation deletion response", "error", err)
	}
}

func parsePositiveInt(value string, fallback, maxValue int) int {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 || parsed > maxValue {
		return fallback
	}
	return parsed
}

func parseNonNegativeInt(value string) int {
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}
