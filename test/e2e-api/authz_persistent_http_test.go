package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/google/uuid"
)

const authzDynamoImage = "public.ecr.aws/aws-dynamodb-local/aws-dynamodb-local@sha256:744bf7138f0e893a6438de89bc3374501105b42bc7473cef6d561f30ff2dec6a"

type authzPolicy struct {
	ID, Content, Revision string
	OwnerAccountID        string `json:"ownerAccountId"`
}

type authzAttachment struct {
	ID, Revision, Scope, Region string
}

type authzGrants struct {
	policies    map[string]authzPolicy
	attachments map[string]authzAttachment
}

type authzReadGate struct {
	loaded, release chan struct{}
}

type authzDynamoProxy struct {
	mu      sync.Mutex
	pending *authzReadGate
}

// The gate holds an already-read DynamoDB response, not a synthetic authorization result.
func (proxy *authzDynamoProxy) freeze() *authzReadGate {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	gate := &authzReadGate{loaded: make(chan struct{}), release: make(chan struct{})}
	proxy.pending = gate
	return gate
}

func authzContainer(t *testing.T, port string, args ...string) string {
	t.Helper()
	command := append([]string{"run", "--pull=never", "-d", "--rm", "-p", "127.0.0.1::" + port}, args...)
	out, err := exec.Command("podman", command...).CombinedOutput()
	if err != nil {
		t.Fatalf("start container: %v\n%s", err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		if out, err := exec.Command("podman", "stop", "--time", "2", id).CombinedOutput(); err != nil {
			t.Errorf("stop owned container %s: %v %s", id, err, out)
		}
	})
	out, err = exec.Command("podman", "port", id, port+"/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("container port: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func authzDynamoCall(endpoint, operation string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-amz-json-1.0")
	request.Header.Set("X-Amz-Target", "DynamoDB_20120810."+operation)
	if err := v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: "local", SecretAccessKey: "local"}, request, fmt.Sprintf("%x", sha256.Sum256(body)), "dynamodb", authzRegion, time.Now()); err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("DynamoDB %s HTTP %d: %s", operation, response.StatusCode, content)
	}
	return nil
}

func authzDynamo(t *testing.T) (string, *authzDynamoProxy) {
	t.Helper()
	endpoint := "http://" + authzContainer(t, "8000", authzDynamoImage, "-jar", "DynamoDBLocal.jar", "-inMemory", "-sharedDb")
	var err error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		err = authzDynamoCall(endpoint, "ListTables", struct{}{})
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"authz-policies", "authz-global", "authz-regional"} {
		if err := authzDynamoCall(endpoint, "CreateTable", map[string]any{
			"TableName": table, "BillingMode": "PAY_PER_REQUEST",
			"AttributeDefinitions": []map[string]string{{"AttributeName": "pk", "AttributeType": "S"}, {"AttributeName": "sk", "AttributeType": "S"}},
			"KeySchema":            []map[string]string{{"AttributeName": "pk", "KeyType": "HASH"}, {"AttributeName": "sk", "KeyType": "RANGE"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &authzDynamoProxy{}
	handler := httputil.NewSingleHostReverseProxy(target)
	handler.ModifyResponse = func(response *http.Response) error {
		if response.Request.Header.Get("X-Amz-Target") != "DynamoDB_20120810.BatchGetItem" {
			return nil
		}
		proxy.mu.Lock()
		gate := proxy.pending
		proxy.pending = nil
		proxy.mu.Unlock()
		if gate == nil {
			return nil
		}
		content, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			return err
		}
		response.Body = io.NopCloser(bytes.NewReader(content))
		close(gate.loaded)
		select {
		case <-gate.release:
			return nil
		case <-response.Request.Context().Done():
			return response.Request.Context().Err()
		}
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL, proxy
}

// authzAdmin is a session of the admin role that authzSeedAdmin grants management.
func authzAdmin(account string) string {
	return "arn:aws:sts::" + account + ":assumed-role/admin/session"
}

// authzSeedAdmin writes the management grant directly into DynamoDB, in the row format pre-prod CI uses.
func authzSeedAdmin(t *testing.T, endpoint, account string) {
	t.Helper()
	const id = "seed-admin"
	policyRevision, attachmentRevision := uuid.NewString(), uuid.NewString()
	policy, _ := json.Marshal(map[string]string{"ID": id, "OwnerAccountID": account, "Content": `permit(principal, action in Seaplane::Action::"PolicyAdmin", resource);`, "Revision": policyRevision, "Kind": "custom"})
	attachment, _ := json.Marshal(map[string]any{"Policy": map[string]string{"ID": id, "Kind": "custom"}, "PrincipalARN": "arn:aws:iam::" + account + ":role/admin", "Scope": "global", "ID": id, "OwnerAccountID": account, "Region": "", "Revision": attachmentRevision})
	put := func(table, pk, sk, revision string, record []byte) map[string]any {
		item := map[string]any{}
		for name, value := range map[string]string{"pk": pk, "sk": sk, "revision": revision, "record": string(record)} {
			item[name] = map[string]string{"S": value}
		}
		return map[string]any{"Put": map[string]any{"TableName": table, "Item": item, "ConditionExpression": "attribute_not_exists(pk)"}}
	}
	accountKey := "ACCOUNT#" + account
	if err := authzDynamoCall(endpoint, "TransactWriteItems", map[string]any{"TransactItems": []any{
		put("authz-policies", accountKey, "POLICY#"+id, policyRevision, policy),
		put("authz-global", accountKey, "ATTACHMENT#"+id, attachmentRevision, attachment),
		put("authz-global", accountKey+"#ROLE#aws/"+account+"/admin", "ATTACHMENT#"+id, attachmentRevision, attachment),
	}}); err != nil {
		t.Fatal(err)
	}
}

func authzManagementRequest(t *testing.T, p *authzProcess, account, method, path, revision string, payload any, want int) *APIResponse {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, p.apiURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Amz-Account-Id", account)
	request.Header.Set("X-Amz-Caller-Arn", authzAdmin(account))
	request.Header.Set("Content-Type", "application/json")
	if revision != "" {
		request.Header.Set("If-Match", `"`+revision+`"`)
	}
	response := authzRawRequest(t, request)
	if response.StatusCode != want {
		t.Fatalf("management %s %s HTTP %d want %d: %s", method, path, response.StatusCode, want, response.Body)
	}
	return response
}

func authzSeedGrants(t *testing.T, p *authzProcess, content []byte) authzGrants {
	t.Helper()
	var fixture struct {
		Policies    []authzPolicy
		Attachments []struct{ ID, PolicyID, PrincipalARN, Scope string }
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	grants := authzGrants{policies: map[string]authzPolicy{}, attachments: map[string]authzAttachment{}}
	for _, spec := range fixture.Policies {
		response := authzManagementRequest(t, p, spec.OwnerAccountID, "POST", "/api/v0/authz/policies", "", map[string]string{"id": spec.ID, "content": spec.Content}, http.StatusCreated)
		var stored authzPolicy
		if err := json.Unmarshal(response.Body, &stored); err != nil || uuid.Validate(stored.Revision) != nil {
			t.Fatalf("stored policy: %s %v", response.Body, err)
		}
		grants.policies[spec.ID] = stored
	}
	for _, spec := range fixture.Attachments {
		policy := grants.policies[spec.PolicyID]
		response := authzManagementRequest(t, p, policy.OwnerAccountID, "POST", "/api/v0/authz/attachments", "", map[string]any{"policy": map[string]string{"id": policy.ID, "type": "custom"}, "principalArn": spec.PrincipalARN, "scope": spec.Scope}, http.StatusCreated)
		var stored authzAttachment
		if err := json.Unmarshal(response.Body, &stored); err != nil || uuid.Validate(stored.ID) != nil || uuid.Validate(stored.Revision) != nil {
			t.Fatalf("stored attachment: %s %v", response.Body, err)
		}
		grants.attachments[spec.ID] = stored
	}
	return grants
}

func authzLiveGrants(t *testing.T, p *authzProcess, proxy *authzDynamoProxy, grants authzGrants) {
	policy := grants.policies["describe"]
	check := func(want int) {
		t.Helper()
		outcome := "allow"
		if want == http.StatusForbidden {
			outcome = "deny"
		}
		authzAssertStatus(t, authzMeasuredGet(t, p, authzClustersPath+"/"+authzStoredClusters[1].name, authzUser("describe-only"), authzAccount, "DescribeCluster", outcome), want)
	}
	check(http.StatusOK)
	gate := proxy.freeze()
	var once sync.Once
	release := func() { once.Do(func() { close(gate.release) }) }
	defer release()
	request, err := http.NewRequest("GET", p.apiURL+authzClustersPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The frozen request loads the same policy as the Describe caller, but is denied List.
	request.Header.Set("X-Amz-Account-Id", authzAccount)
	request.Header.Set("X-Amz-Caller-Arn", authzUser("describe-only"))
	responses := make(chan *http.Response, 1)
	errors := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
		if err != nil {
			errors <- err
			return
		}
		responses <- response
	}()
	select {
	case <-gate.loaded:
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("request did not load persistent policy snapshot")
	}
	updated := authzManagementRequest(t, p, authzAccount, "PUT", "/api/v0/authz/policies/"+policy.ID, policy.Revision, map[string]string{"content": `permit(principal, action == Seaplane::Action::"ListClusters", resource is Seaplane::Collection);`}, http.StatusOK)
	var replacement authzPolicy
	if err := json.Unmarshal(updated.Body, &replacement); err != nil || replacement.Revision == policy.Revision {
		t.Fatalf("policy edit lost revision: %s %v", updated.Body, err)
	}
	authzManagementRequest(t, p, authzAccount, "PUT", "/api/v0/authz/policies/"+policy.ID, policy.Revision, map[string]string{"content": policy.Content}, http.StatusConflict)
	release()
	select {
	case response := <-responses:
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusForbidden {
			t.Fatalf("frozen request must retain old deny: HTTP %d %s %v", response.StatusCode, body, err)
		}
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("frozen request did not complete")
	}
	check(http.StatusForbidden)
	authzAssertStatus(t, authzMeasuredGet(t, p, authzClustersPath, authzUser("describe-only"), authzAccount, "ListClusters", "allow"), http.StatusOK)
	attachment := grants.attachments["describe-only"]
	path := "/api/v0/authz/attachments/" + attachment.ID + "?scope=" + attachment.Scope
	authzManagementRequest(t, p, authzAccount, "DELETE", path, uuid.NewString(), nil, http.StatusConflict)
	authzManagementRequest(t, p, authzAccount, "DELETE", path, attachment.Revision, nil, http.StatusOK)
	authzAssertStatus(t, authzMeasuredGet(t, p, authzClustersPath, authzUser("describe-only"), authzAccount, "ListClusters", "deny"), http.StatusForbidden)
}
