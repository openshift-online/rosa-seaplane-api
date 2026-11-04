package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestManagementMalformedHTTP rejects injected fields, trailing JSON, and invalid preconditions before service calls.
func TestManagementMalformedHTTP(t *testing.T) {
	h := NewManagementHandler(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tc := range []struct {
		name, method, path, body, match string
		call                            http.HandlerFunc
	}{
		{"unknown owner", "POST", "/", `{"id":"x","content":"x","ownerAccountId":"other"}`, "", h.CreatePolicy},
		{"trailing", "POST", "/", `{"id":"x","content":"x"} {}`, "", h.CreatePolicy},
		{"null", "POST", "/", `null`, "", h.CreatePolicy},
		{"attachment region", "POST", "/", `{"policy":{"id":"x","type":"custom"},"principalArn":"x","region":"us-west-2"}`, "", h.CreateAttachment},
		{"nested injection", "POST", "/", `{"policy":{"id":"x","type":"custom","ownerAccountId":"x"},"principalArn":"x"}`, "", h.CreateAttachment},
		{"missing revision", "PUT", "/", `{"content":"x"}`, "", h.UpdatePolicy},
		{"unquoted revision", "PUT", "/", `{"content":"x"}`, "550e8400-e29b-41d4-a716-446655440000", h.UpdatePolicy},
		{"weak revision", "DELETE", "/", "", `W/"550e8400-e29b-41d4-a716-446655440000"`, h.DeletePolicy},
		{"revision list", "DELETE", "/", "", `"550e8400-e29b-41d4-a716-446655440000", "550e8400-e29b-41d4-a716-446655440000"`, h.DeletePolicy},
		{"bad kind", "GET", "/?type=other", "", "", h.GetPolicy},
		{"bad scope", "GET", "/?scope=other", "", "", h.ListAttachments},
		{"missing delete scope", "DELETE", "/", "", `"550e8400-e29b-41d4-a716-446655440000"`, h.DeleteAttachment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("If-Match", tc.match)
			r = mux.SetURLVars(r, map[string]string{"id": "x"})
			w := httptest.NewRecorder()
			tc.call(w, r)
			var status metav1.Status
			if w.Code != 400 || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Kind != "Status" {
				t.Fatalf("got %d %s", w.Code, w.Body)
			}
		})
	}
}

// TestManagementErrorPrivacy keeps submitted policy content out of responses and logs.
func TestManagementErrorPrivacy(t *testing.T) {
	var logs bytes.Buffer
	h := NewManagementHandler(nil, slog.New(slog.NewTextHandler(&logs, nil)))
	w := httptest.NewRecorder()
	content := "sensitive-policy-source"
	h.writeError(w, fmt.Errorf("%w: %s", authz.ErrValidation, content))
	if strings.Contains(logs.String(), content) || strings.Contains(w.Body.String(), content) {
		t.Fatalf("policy content leaked: logs=%s response=%s", &logs, w.Body)
	}
}

// TestManagementErrors preserves typed service failures and writer guidance as structured Status responses.
func TestManagementErrors(t *testing.T) {
	h := NewManagementHandler(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tc := range []struct {
		err  error
		code int
	}{
		{authz.ErrValidation, 422}, {authz.ErrManagementDenied, 403}, {authz.ErrManagedPolicy, 403},
		{authz.ErrNotFound, 404}, {authz.ErrConflict, 409}, {authz.ErrUnavailable, 503},
		{&authz.WrongWriterError{Region: "us-west-2"}, 409},
		{&authz.Failure{}, 500},
		{&authz.Failure{Err: authz.ErrNotFound}, 500},
		{&authz.Failure{Err: authz.ErrValidation}, 500},
		{&authz.Failure{Err: authz.ErrConflict}, 500},
	} {
		w := httptest.NewRecorder()
		h.writeError(w, tc.err)
		if w.Code != tc.code {
			t.Fatalf("%v: %d %s", tc.err, w.Code, w.Body)
		}
		if _, ok := tc.err.(*authz.WrongWriterError); ok {
			var status metav1.Status
			_ = json.Unmarshal(w.Body.Bytes(), &status)
			if w.Header().Get("X-Authz-Writer-Region") != "us-west-2" || status.Details == nil || status.Details.Causes[0].Field != "writerRegion" {
				t.Fatalf("missing guidance: %s", w.Body)
			}
		}
	}
}
