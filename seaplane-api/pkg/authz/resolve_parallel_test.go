package authz_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

// An assumed-role caller needs four attachment queries (two scopes, session and
// role). The fake holds each query until all four are in flight, so sequential
// resolution can never pass.
func TestPrepareQueriesInParallel(t *testing.T) {
	const want = 4
	var mu sync.Mutex
	inFlight, peak := 0, 0
	allIn := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if !strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".Query") {
			t.Errorf("unexpected operation %s", r.Header.Get("X-Amz-Target"))
		}
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		if inFlight == want {
			close(allIn)
		}
		mu.Unlock()
		select {
		case <-allIn:
		case <-time.After(2 * time.Second):
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = io.WriteString(w, `{"Items":[],"Count":0}`)
	}))
	defer endpoint.Close()
	client := dynamodb.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""), RetryMaxAttempts: 1}, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint.URL) })
	store, err := authz.NewTestStore(client, authz.StoreConfig{AccountID: "123456789012", Region: "us-east-1", PolicyTable: "policies", GlobalTable: "global", RegionalTable: "regional"})
	if err != nil {
		t.Fatal(err)
	}

	id := authz.Identity{AccountID: "123456789012", CallerARN: "arn:aws:sts::123456789012:assumed-role/readers/session-a"}
	if _, err := store.Prepare(t.Context(), id, authz.RequestContext{RequestTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if peak != want {
		t.Fatalf("peak concurrent attachment queries = %d, want %d", peak, want)
	}
}
