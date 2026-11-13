package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/api"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
)

var (
	errAuthzDenied = api.APIError{Code: "AUTHZ-DENIED-001", HTTPStatus: http.StatusForbidden, Message: "Access denied"}
	errAuthzFailed = api.APIError{Code: "AUTHZ-FAILED-001", HTTPStatus: http.StatusInternalServerError, Message: "Authorization failed"}
)

func writeResourceAuthzError(w http.ResponseWriter, r *http.Request, attempt *authz.Attempt, operation authz.Action, err error, logger *slog.Logger) {
	stage, action := authz.StageEvaluation, operation
	attrs := []any{
		"operation", operation,
		"account_id", middleware.GetAccountID(r.Context()),
		"caller_arn", middleware.GetCallerARN(r.Context()),
		"error", err,
	}
	// Failure.Error deliberately hides the diagnostics. Log their internal fields separately.
	var failure *authz.Failure
	if errors.As(err, &failure) {
		stage = failure.Stage
		if failure.Action != "" {
			action = failure.Action
		}
		attrs = append(attrs, "cause", failure.Err, "provenance", failure.Provenance, "diagnostics", failure.Diagnostics)
	}
	attrs = append(attrs, "stage", stage, "action", action)
	logger.Error("request authorization failed", attrs...)
	attempt.Finish(authz.OutcomeError, stage)
	writeAPIError(w, errAuthzFailed, logger)
}

// writeAuthzDenied logs an ordinary denial so a 403 can be explained. Provenance
// carries policy and attachment IDs only, never policy content.
func writeAuthzDenied(w http.ResponseWriter, r *http.Request, attempt *authz.Attempt, operation, action authz.Action, decision authz.Decision, logger *slog.Logger) {
	logger.Info("request authorization denied",
		"operation", operation,
		"action", action,
		"account_id", middleware.GetAccountID(r.Context()),
		"caller_arn", middleware.GetCallerARN(r.Context()),
		"provenance", decision.Provenance,
	)
	attempt.Finish(authz.OutcomeDeny, authz.StageNone)
	writeAPIError(w, errAuthzDenied, logger)
}

type requestLabelsKey struct{}

// withRequestLabels sets the customer labels the request writes, Cedar's context.requestLabels.
func withRequestLabels(r *http.Request, labels map[string]string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestLabelsKey{}, labels))
}

func prepareResourceAuthz(r *http.Request, authorizer authz.Resolver) (*authz.Prepared, error) {
	shared, _ := r.Context().Value(authzOutcomeKey{}).(*requestAuthz)
	if shared != nil && shared.prepared != nil {
		return shared.prepared, nil
	}
	requestLabels, _ := r.Context().Value(requestLabelsKey{}).(map[string]string)
	prepared, err := authorizer.Prepare(r.Context(), authz.Identity{
		AccountID: middleware.GetAccountID(r.Context()), CallerARN: middleware.GetCallerARN(r.Context()),
	}, authz.RequestContext{SourceIP: middleware.GetSourceIP(r.Context()), UserAgent: r.UserAgent(), RequestTime: time.Now(), RequestLabels: requestLabels})
	if shared != nil && err == nil {
		shared.prepared = prepared
	}
	return prepared, err
}

// authorizeCollection prepares one frozen policy/context snapshot for all item checks.
func authorizeCollection(w http.ResponseWriter, r *http.Request, authorizer authz.Resolver, logger *slog.Logger, operation authz.Action, resource authz.Resource) (*authz.Prepared, *authz.Attempt) {
	attempt := authz.DefaultMetrics.Start(operation)
	prepared, err := prepareResourceAuthz(r, authorizer)
	if err != nil {
		writeResourceAuthzError(w, r, attempt, operation, err, logger)
		return nil, nil
	}
	decision, err := prepared.Check(r.Context(), operation, resource)
	if err != nil {
		writeResourceAuthzError(w, r, attempt, operation, err, logger)
		return nil, nil
	}
	if !decision.Allowed {
		writeAuthzDenied(w, r, attempt, operation, operation, decision, logger)
		return nil, nil
	}
	return prepared, attempt
}

type authzOutcomeKey struct{}

type requestAuthz struct {
	attempt  *authz.Attempt
	prepared *authz.Prepared
}

// Retried writes share one terminal outcome and frozen context, but not resource facts.
func withAuthzOutcome(r *http.Request) (*http.Request, func()) {
	shared := &requestAuthz{}
	return r.WithContext(context.WithValue(r.Context(), authzOutcomeKey{}, shared)), func() {
		if shared.attempt != nil {
			shared.attempt.Finish(authz.OutcomeAllow, authz.StageNone)
		}
	}
}

func startResourceAuthz(r *http.Request, operation authz.Action) *authz.Attempt {
	shared, _ := r.Context().Value(authzOutcomeKey{}).(*requestAuthz)
	if shared == nil {
		return authz.DefaultMetrics.Start(operation)
	}
	if shared.attempt == nil {
		shared.attempt = authz.DefaultMetrics.Start(operation)
	}
	return shared.attempt
}

// authorizeResource checks the base operation and every effective changed-field action before a write.
func authorizeResource(w http.ResponseWriter, r *http.Request, authorizer authz.Resolver, logger *slog.Logger, operation authz.Action, resource authz.Resource, additional ...authz.Action) bool {
	attempt := startResourceAuthz(r, operation)
	prepared, err := prepareResourceAuthz(r, authorizer)
	if err != nil {
		writeResourceAuthzError(w, r, attempt, operation, err, logger)
		return false
	}
	for _, action := range append([]authz.Action{operation}, additional...) {
		decision, err := prepared.Check(r.Context(), action, resource)
		if err != nil {
			writeResourceAuthzError(w, r, attempt, operation, err, logger)
			return false
		}
		if !decision.Allowed {
			writeAuthzDenied(w, r, attempt, operation, action, decision, logger)
			return false
		}
	}
	if r.Context().Value(authzOutcomeKey{}) == nil {
		attempt.Finish(authz.OutcomeAllow, authz.StageNone)
	}
	return true
}
