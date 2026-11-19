package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/api"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

type contextKey string

const (
	// ContextKeyAccountID is the context key for AWS account ID
	ContextKeyAccountID contextKey = "account_id"
	// ContextKeyCallerARN is the context key for AWS caller ARN
	ContextKeyCallerARN contextKey = "caller_arn"
	// ContextKeyUserID is the context key for AWS user ID
	ContextKeyUserID contextKey = "user_id"
	// ContextKeySourceIP is the context key for source IP
	ContextKeySourceIP contextKey = "source_ip"
	// ContextKeyRequestID is the context key for request ID
	ContextKeyRequestID contextKey = "request_id"
)

// AWS identity headers from API Gateway
const (
	HeaderAccountID = "X-Amz-Account-Id"
	HeaderCallerARN = "X-Amz-Caller-Arn"
	HeaderUserID    = "X-Amz-User-Id"
	HeaderSourceIP  = "X-Amz-Source-Ip"
	HeaderRequestID = "X-Amz-Request-Id"
)

// Identity extracts AWS identity headers and adds them to the request context
func Identity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if accountID := r.Header.Get(HeaderAccountID); accountID != "" {
			ctx = context.WithValue(ctx, ContextKeyAccountID, accountID)
		}

		if callerARN := r.Header.Get(HeaderCallerARN); callerARN != "" {
			ctx = context.WithValue(ctx, ContextKeyCallerARN, callerARN)
		}

		if userID := r.Header.Get(HeaderUserID); userID != "" {
			ctx = context.WithValue(ctx, ContextKeyUserID, userID)
		}

		if sourceIP := r.Header.Get(HeaderSourceIP); sourceIP != "" {
			ctx = context.WithValue(ctx, ContextKeySourceIP, sourceIP)
		}

		if requestID := r.Header.Get(HeaderRequestID); requestID != "" {
			ctx = context.WithValue(ctx, ContextKeyRequestID, requestID)
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func RequireIdentity(logger *slog.Logger, isAccountRegistered func(context.Context, string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v0/live", "/api/v0/ready", "/api/v0/info":
				next.ServeHTTP(w, r)
				return
			}
			accountID := GetAccountID(r.Context())
			if accountID == "" || GetCallerARN(r.Context()) == "" {
				if err := api.WriteError(w, api.APIError{
					Code: "AUTH-001", HTTPStatus: http.StatusForbidden, Message: "Caller account ID and ARN are required",
				}); err != nil {
					logger.Error("failed to write identity error", "error", err)
				}
				return
			}
			// Reject inconsistent gateway identity before enrollment or grant selection.
			identity := authz.Identity{AccountID: accountID, CallerARN: GetCallerARN(r.Context())}
			if err := identity.Validate(); err != nil {
				logger.Warn("invalid caller identity", "error", err)
				if err := api.WriteError(w, api.APIError{
					Code: "AUTH-001", HTTPStatus: http.StatusForbidden, Message: "Caller identity is invalid",
				}); err != nil {
					logger.Error("failed to write identity error", "error", err)
				}
				return
			}
			if !isAccountRegistered(r.Context(), accountID) {
				if err := api.WriteError(w, api.APIError{
					Code: "AUTH-002", HTTPStatus: http.StatusForbidden, Message: "Account is not registered",
				}); err != nil {
					logger.Error("failed to write registration error", "error", err)
				}
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// GetAccountID retrieves the AWS account ID from context
func GetAccountID(ctx context.Context) string {
	if v := ctx.Value(ContextKeyAccountID); v != nil {
		return v.(string)
	}
	return ""
}

// GetCallerARN retrieves the AWS caller ARN from context
func GetCallerARN(ctx context.Context) string {
	if v := ctx.Value(ContextKeyCallerARN); v != nil {
		return v.(string)
	}
	return ""
}

// GetSourceIP retrieves the gateway-supplied source address, never a forwarding header.
func GetSourceIP(ctx context.Context) string {
	if v, ok := ctx.Value(ContextKeySourceIP).(string); ok {
		return v
	}
	return ""
}

// GetUserID retrieves the AWS user ID from context
func GetUserID(ctx context.Context) string {
	if v := ctx.Value(ContextKeyUserID); v != nil {
		return v.(string)
	}
	return ""
}

// GetRequestID retrieves the request ID from context
func GetRequestID(ctx context.Context) string {
	if v := ctx.Value(ContextKeyRequestID); v != nil {
		return v.(string)
	}
	return ""
}
