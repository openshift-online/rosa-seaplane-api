package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/api"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
)

// ManagementHandler adapts the management service to the public HTTP contract.
type ManagementHandler struct {
	management *authz.Management
	logger     *slog.Logger
}

type policyReference struct {
	ID   string           `json:"id"`
	Type authz.PolicyKind `json:"type"`
}
type policyResponse struct {
	ID             string           `json:"id"`
	Type           authz.PolicyKind `json:"type"`
	OwnerAccountID string           `json:"ownerAccountId"`
	Content        string           `json:"content"`
	Revision       string           `json:"revision"`
}
type attachmentResponse struct {
	ID             string                `json:"id"`
	OwnerAccountID string                `json:"ownerAccountId"`
	Policy         policyReference       `json:"policy"`
	PrincipalARN   string                `json:"principalArn"`
	Scope          authz.AttachmentScope `json:"scope"`
	Region         string                `json:"region,omitempty"`
	Revision       string                `json:"revision"`
}
type managementList[T any] struct {
	Kind  string `json:"kind"`
	Total int    `json:"total"`
	Items []T    `json:"items"`
}

// NewManagementHandler returns HTTP handlers backed by the account-scoped management service.
func NewManagementHandler(management *authz.Management, logger *slog.Logger) *ManagementHandler {
	return &ManagementHandler{management: management, logger: logger}
}

// managementCaller captures gateway identity and request facts without legacy allowlist privileges.
func managementCaller(r *http.Request) authz.ManagementCaller {
	return authz.ManagementCaller{AccountID: middleware.GetAccountID(r.Context()), CallerARN: middleware.GetCallerARN(r.Context()), Request: authz.RequestContext{SourceIP: middleware.GetSourceIP(r.Context()), UserAgent: r.UserAgent(), RequestTime: time.Now()}}
}

// decodeManagement rejects unknown fields, null bodies, and additional JSON values.
func decodeManagement(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	if string(raw) == "null" {
		return errors.New("JSON object is required")
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("single JSON object is required")
	}
	return nil
}

// managementRef parses the policy type while keeping account ownership server-assigned.
func managementRef(r *http.Request) (authz.PolicyRef, error) {
	kind := authz.PolicyKind(r.URL.Query().Get("type"))
	if kind == "" {
		kind = authz.CustomPolicy
	}
	if kind != authz.CustomPolicy && kind != authz.ManagedPolicy {
		return authz.PolicyRef{}, errors.New("type must be custom or managed")
	}
	return authz.PolicyRef{ID: mux.Vars(r)["id"], Kind: kind}, nil
}

// managementScope accepts explicit global or regional scope, or omission for both.
func managementScope(r *http.Request) (authz.AttachmentScope, error) {
	scope := authz.AttachmentScope(r.URL.Query().Get("scope"))
	if scope == authz.GlobalScope || scope == authz.RegionalScope || scope == "" {
		return scope, nil
	}
	return "", errors.New("scope must be global or regional")
}

// managementRevision requires exactly one strong quoted nonzero UUID revision.
func managementRevision(r *http.Request) (string, error) {
	values := r.Header.Values("If-Match")
	if len(values) != 1 {
		return "", errors.New("If-Match must contain one quoted UUID")
	}
	value := values[0]
	if len(value) != 38 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", errors.New("If-Match must contain one quoted UUID")
	}
	revision := value[1 : len(value)-1]
	if uuid.Validate(revision) != nil || revision == uuid.Nil.String() {
		return "", errors.New("If-Match must contain one nonzero quoted UUID")
	}
	return revision, nil
}

// publicPolicy maps a stored policy to the public response shape.
func publicPolicy(p authz.StoredPolicy) policyResponse {
	return policyResponse{ID: p.ID, Type: p.Kind, OwnerAccountID: p.OwnerAccountID, Content: p.Content, Revision: p.Revision}
}

// publicAttachment maps a stored attachment without exposing internal storage keys.
func publicAttachment(a authz.StoredAttachment) attachmentResponse {
	return attachmentResponse{ID: a.ID, OwnerAccountID: a.OwnerAccountID, Policy: policyReference{ID: a.Policy.ID, Type: a.Policy.Kind}, PrincipalARN: a.PrincipalARN, Scope: a.Scope, Region: a.Region, Revision: a.Revision}
}

// badRequest writes a structured malformed-input error.
func (h *ManagementHandler) badRequest(w http.ResponseWriter, err error) {
	writeAPIError(w, api.APIError{Code: "AUTHZ-INPUT-001", HTTPStatus: 400, Message: err.Error()}, h.logger)
}

// writeError maps typed service failures without classifying backend error strings.
func (h *ManagementHandler) writeError(w http.ResponseWriter, err error) {
	status, message := 500, "Authorization management failed"
	var writer *authz.WrongWriterError
	_, isFailure := errors.AsType[*authz.Failure](err)
	switch {
	case isFailure:
	case errors.Is(err, authz.ErrManagementDenied):
		status, message = 403, "Access denied"
	case errors.Is(err, authz.ErrManagedPolicy):
		status, message = 403, "Managed policies are immutable"
	case errors.Is(err, authz.ErrValidation):
		status, message = 422, "Invalid authorization input"
	case errors.Is(err, authz.ErrNotFound):
		status, message = 404, "Authorization record not found"
	case errors.Is(err, authz.ErrWrongWriter):
		status, message = 409, "Global mutation requires the account writer region"
	case errors.Is(err, authz.ErrConflict):
		status, message = 409, "Authorization record revision conflict"
	case errors.Is(err, authz.ErrUnavailable):
		status, message = 503, "Authorization backend unavailable"
	}
	def := api.APIError{Code: "AUTHZ-MANAGEMENT-001", HTTPStatus: status, Message: message}
	if errors.As(err, &writer) {
		w.Header().Set("X-Authz-Writer-Region", writer.Region)
		def = def.WithErrors([]map[string]string{{"field": "writerRegion", "detail": writer.Region}})
	}
	h.logger.Error("authorization management request failed", "status", status, "reason", message)
	writeAPIError(w, def, h.logger)
}

// respond writes a committed result or a structured service failure.
func (h *ManagementHandler) respond(w http.ResponseWriter, status int, value any, err error) {
	if err != nil {
		h.writeError(w, err)
		return
	}
	if err := api.Write(w, status, value); err != nil {
		h.logger.Error("failed to write management response", "error", err)
	}
}

// CreatePolicy creates a custom policy with ownership inferred from gateway identity.
func (h *ManagementHandler) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	if err := decodeManagement(r, &input); err != nil {
		h.badRequest(w, err)
		return
	}
	p, err := h.management.CreatePolicy(r.Context(), managementCaller(r), input.ID, input.Content)
	h.respond(w, 201, publicPolicy(p), err)
}

// ListPolicies returns custom and managed policies available to the caller's account.
func (h *ManagementHandler) ListPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := h.management.ListPolicies(r.Context(), managementCaller(r))
	items := make([]policyResponse, 0, len(policies))
	for _, p := range policies {
		items = append(items, publicPolicy(p))
	}
	h.respond(w, 200, managementList[policyResponse]{Kind: "PolicyList", Total: len(items), Items: items}, err)
}

// GetPolicy describes a custom or managed policy selected by type.
func (h *ManagementHandler) GetPolicy(w http.ResponseWriter, r *http.Request) {
	ref, err := managementRef(r)
	if err != nil {
		h.badRequest(w, err)
		return
	}
	p, err := h.management.GetPolicy(r.Context(), managementCaller(r), ref)
	h.respond(w, 200, publicPolicy(p), err)
}

// UpdatePolicy replaces a custom policy only when its revision matches.
func (h *ManagementHandler) UpdatePolicy(w http.ResponseWriter, r *http.Request) {
	ref, err := managementRef(r)
	if err != nil {
		h.badRequest(w, err)
		return
	}
	if ref.Kind == authz.ManagedPolicy {
		h.writeError(w, authz.ErrManagedPolicy)
		return
	}
	revision, err := managementRevision(r)
	if err != nil {
		h.badRequest(w, err)
		return
	}
	var input struct {
		Content string `json:"content"`
	}
	if err := decodeManagement(r, &input); err != nil {
		h.badRequest(w, err)
		return
	}
	p, err := h.management.UpdatePolicy(r.Context(), managementCaller(r), ref, revision, input.Content)
	h.respond(w, 200, publicPolicy(p), err)
}

// DeletePolicy returns a committed deletion receipt for a revision-matched custom policy.
func (h *ManagementHandler) DeletePolicy(w http.ResponseWriter, r *http.Request) {
	ref, err := managementRef(r)
	if err != nil {
		h.badRequest(w, err)
		return
	}
	if ref.Kind == authz.ManagedPolicy {
		h.writeError(w, authz.ErrManagedPolicy)
		return
	}
	revision, err := managementRevision(r)
	if err != nil {
		h.badRequest(w, err)
		return
	}
	receipt, err := h.management.DeletePolicy(r.Context(), managementCaller(r), ref, revision)
	h.respond(w, 200, receipt, err)
}

// CreateAttachment attaches a policy globally by default or in the current region.
func (h *ManagementHandler) CreateAttachment(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Policy       policyReference       `json:"policy"`
		PrincipalARN string                `json:"principalArn"`
		Scope        authz.AttachmentScope `json:"scope"`
	}
	if err := decodeManagement(r, &input); err != nil {
		h.badRequest(w, err)
		return
	}
	if input.Scope == "" {
		input.Scope = authz.GlobalScope
	}
	a, err := h.management.CreateAttachment(r.Context(), managementCaller(r), authz.AttachmentSpec{Policy: authz.PolicyRef{ID: input.Policy.ID, Kind: input.Policy.Type}, PrincipalARN: input.PrincipalARN, Scope: input.Scope})
	h.respond(w, 201, publicAttachment(a), err)
}

// ListAttachments lists the requested scope or requires permission for both scopes when omitted.
func (h *ManagementHandler) ListAttachments(w http.ResponseWriter, r *http.Request) {
	scope, err := managementScope(r)
	if err != nil {
		h.badRequest(w, err)
		return
	}
	attachments, err := h.management.ListAttachments(r.Context(), managementCaller(r), scope)
	items := make([]attachmentResponse, 0, len(attachments))
	for _, a := range attachments {
		items = append(items, publicAttachment(a))
	}
	h.respond(w, 200, managementList[attachmentResponse]{Kind: "AttachmentList", Total: len(items), Items: items}, err)
}

// DeleteAttachment requires explicit scope and returns a revision-matched deletion receipt.
func (h *ManagementHandler) DeleteAttachment(w http.ResponseWriter, r *http.Request) {
	scope, err := managementScope(r)
	if scope == "" {
		err = errors.New("scope must be global or regional")
	}
	if err != nil {
		h.badRequest(w, err)
		return
	}
	revision, err := managementRevision(r)
	if err != nil {
		h.badRequest(w, err)
		return
	}
	receipt, err := h.management.DeleteAttachment(r.Context(), managementCaller(r), scope, mux.Vars(r)["id"], revision)
	h.respond(w, 200, receipt, err)
}
