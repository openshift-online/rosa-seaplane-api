package handlers

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
)

func TestRegionalIdentityMetadata(t *testing.T) {
	identity := config.NewConfig().CustomerAccess
	identity.IssuerURL, identity.Subject = "https://issuer.example/id", "system:serviceaccount:platform-api:platform-api-sa"
	for _, tc := range []struct {
		policy string
		status int
	}{{clusterReadPermit, 200}, {clusterListPermit, 403}} {
		h := NewRegionalIdentityHandler(identity, clusterAuthorizer(t, tc.policy), slog.New(slog.NewTextHandler(io.Discard, nil)))
		response := httptest.NewRecorder()
		h.Get(response, httptest.NewRequest(http.MethodGet, "/api/v0/regional_identity", nil).WithContext(testContext(testAccountID)))
		if response.Code != tc.status {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		if response.Code == 200 {
			var metadata struct {
				AdditionalTrustStatement struct {
					Principal map[string]string
					Condition map[string]map[string]string
				}
			}
			if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata.AdditionalTrustStatement.Principal["Federated"] != "arn:aws:iam::"+testAccountID+":oidc-provider/issuer.example/id" || metadata.AdditionalTrustStatement.Condition["StringEquals"]["issuer.example/id:sub"] != identity.Subject || strings.Contains(response.Body.String(), identity.TokenFile) {
				t.Fatalf("invalid metadata: %s", response.Body.String())
			}
		}
	}
}
