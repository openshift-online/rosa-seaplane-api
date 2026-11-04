package handlers

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/api"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
)

type RegionalIdentityHandler struct {
	identity   config.CustomerAccessConfig
	authorizer *authz.Authorizer
	logger     *slog.Logger
}

func NewRegionalIdentityHandler(identity config.CustomerAccessConfig, authorizer *authz.Authorizer, logger *slog.Logger) *RegionalIdentityHandler {
	return &RegionalIdentityHandler{identity: identity, authorizer: authorizer, logger: logger}
}

// Get publishes customer-specific trust metadata, never tokens or credentials.
func (h *RegionalIdentityHandler) Get(w http.ResponseWriter, r *http.Request) {
	account := middleware.GetAccountID(r.Context())
	prepared, attempt := authorizeCollection(w, r, h.authorizer, h.logger, authz.DescribeRegionalIdentity, authz.Resource{Kind: authz.Collection, CollectionKind: authz.RegionalAccess, AccountID: account})
	if prepared == nil {
		return
	}
	defer attempt.Finish(authz.OutcomeAllow, authz.StageNone)
	issuer := strings.TrimPrefix(h.identity.IssuerURL, "https://")
	statement := map[string]any{
		"Sid": "RegionalPlatformAPIOIDC", "Effect": "Allow",
		"Principal": map[string]string{"Federated": "arn:aws:iam::" + account + ":oidc-provider/" + issuer},
		"Action":    "sts:AssumeRoleWithWebIdentity",
		"Condition": map[string]any{"StringEquals": map[string]string{issuer + ":aud": h.identity.Audience, issuer + ":sub": h.identity.Subject}},
	}
	response := map[string]any{
		"issuerURL": h.identity.IssuerURL, "audience": h.identity.Audience, "contractVersion": h.identity.ContractVersion,
		"workloads":                map[string]any{"platform-api": map[string]string{"subject": h.identity.Subject}},
		"additionalTrustStatement": statement,
	}
	if err := api.Write(w, http.StatusOK, response); err != nil {
		h.logger.Error("failed to write regional identity response", "error", err)
	}
}
