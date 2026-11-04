package customeraws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestValidateRoleARN(t *testing.T) {
	for _, role := range []string{"arn:aws:iam::123456789012:role/Installer", "arn:aws:iam::123456789012:role/team/Installer"} {
		if err := ValidateRoleARN(role, "123456789012"); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"", "arn:aws:iam::999999999999:role/Installer", "arn:aws:sts::123456789012:assumed-role/Installer/session", "arn:aws:iam::123456789012:user/Installer", "arn:aws:iam::123456789012:role/", "arn:aws:iam::123456789012:role/" + strings.Repeat("x", 65)} {
		if err := ValidateRoleARN(role, "123456789012"); err == nil {
			t.Fatalf("accepted %q", role)
		}
	}
}

func TestRegionalCustomerCredentials(t *testing.T) {
	const account = "123456789012"
	const role = "arn:aws:iam::123456789012:role/Installer"
	for _, tc := range []struct {
		name, returnedAccount string
		deny, missingToken    bool
		want                  ErrorKind
	}{
		{"exchange and verify", account, false, false, ""},
		{"account mismatch", "999999999999", false, false, AccountMismatch},
		{"trust denied", account, true, false, TrustFailed},
		{"runtime token unavailable", account, false, true, IdentityUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "token")
			if !tc.missingToken {
				if err := os.WriteFile(file, []byte("projected-token"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			factory := New("us-east-1", file)
			factory.httpClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				params, err := url.ParseQuery(string(body))
				if err != nil {
					return nil, err
				}
				status, response := 200, ""
				switch params.Get("Action") {
				case "AssumeRoleWithWebIdentity":
					if r.Header.Get("Authorization") != "" || params.Get("RoleArn") != role || params.Get("WebIdentityToken") != "projected-token" || params.Get("DurationSeconds") != "900" {
						t.Errorf("incorrect web identity request")
					}
					if tc.deny {
						status, response = 403, `<ErrorResponse><Error><Code>AccessDenied</Code><Message>do not expose this detail</Message></Error></ErrorResponse>`
					} else {
						response = fmt.Sprintf(`<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>ASIATESTKEY</AccessKeyId><SecretAccessKey>test-secret</SecretAccessKey><SessionToken>test-session</SessionToken><Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
					}
				case "GetCallerIdentity":
					if !strings.Contains(r.Header.Get("Authorization"), "ASIATESTKEY") {
						t.Error("account check did not use customer credentials")
					}
					response = fmt.Sprintf(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Account>%s</Account><Arn>arn:aws:sts::%s:assumed-role/Installer/session</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`, tc.returnedAccount, tc.returnedAccount)
				default:
					t.Errorf("unexpected AWS action %q", params.Get("Action"))
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/xml"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			client, err := factory.Get(context.Background(), role, account)
			if tc.want == "" {
				if err != nil || client == nil || client.AccountID != account || calls != 2 {
					t.Fatalf("client=%v err=%v calls=%d", client, err, calls)
				}
				// A second request must perform another exchange, not reuse the first session.
				if _, err := factory.Get(context.Background(), role, account); err != nil || calls != 4 {
					t.Fatalf("fresh exchange: calls=%d err=%v", calls, err)
				}
			} else {
				var accessErr *Error
				if !errors.As(err, &accessErr) || accessErr.Kind != tc.want || client != nil {
					t.Fatalf("client=%v err=%v", client, err)
				}
				if tc.missingToken && calls != 0 {
					t.Fatal("missing token contacted AWS")
				}
			}
		})
	}
}
