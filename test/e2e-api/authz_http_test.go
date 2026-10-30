package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	seaplanev1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	public "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	fleetdb "github.com/openshift-online/rosa-seaplane-api/seaplane-db"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/test/testinfra"
	hypershiftv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	authzAccount      = "111111111111"
	authzOtherAccount = "222222222222"
	authzRegion       = "us-east-1"
	authzAccountLabel = seaplanev1.AccountIDLabel
	authzTeamLabel    = "example.com/team"
	authzFaultLabel   = "example.com/fault"
	authzFaultValue   = "fault-detail-secret"
	authzClustersPath = "/api/v0/clusters"
	authzReadyTimeout = 20 * time.Second
	authzStopTimeout  = 10 * time.Second
)

// id is the seaplane-db-assigned UID, recorded by authzSeedClusters.
var authzStoredClusters = []struct {
	id, name, account, team string
}{
	{name: "hidden-red", account: authzAccount, team: "red"},
	{name: "visible-blue-one", account: authzAccount, team: "blue"},
	{name: "hidden-missing", account: authzAccount},
	{name: "visible-blue-two", account: authzAccount, team: "blue"},
	{name: "foreign-blue", account: authzOtherAccount, team: "blue"},
}

type authzProcess struct {
	cmd                                    *exec.Cmd
	done                                   chan struct{}
	err                                    error
	logPath, apiURL, healthURL, metricsURL string
	stopOnce                               sync.Once
}

func TestAuthzHTTP(t *testing.T) {
	binary := os.Getenv("AUTHZ_HTTP_API_BINARY")
	if binary == "" {
		if os.Getenv("AUTHZ_HTTP_REQUIRED") == "1" {
			t.Fatal("AUTHZ_HTTP_API_BINARY is required for the local authorization target")
		}
		t.Skip("local authorization binary absent; run make test-e2e-authz")
	}
	output := os.Getenv("AUTHZ_HTTP_OUTPUT_DIR")
	if output == "" {
		output = filepath.Join("..", "..", "test-results", fmt.Sprintf("authz-http-%d", time.Now().UnixNano()))
	}
	output, err := filepath.Abs(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	t.Logf("local HTTP artifacts: %s", output)
	if os.Getenv("PGCTL_DSN") != "" {
		t.Fatal("refusing external PGCTL_DSN; only a test-owned disposable database is safe")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(binary); err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		t.Fatalf("API binary is not executable: %s (%v)", binary, err)
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatal("Podman is required by seaplane-db testinfra: ", err)
	}
	authzClearAWS(t)
	bundle, err := os.ReadFile("testdata/authz-http.json")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(output, "authz-config.json")

	db := testinfra.StartPostgres(t)
	start := func(t *testing.T, name string) *authzProcess {
		t.Helper()
		return authzStartProcess(t, binary, configPath, db.ConnStr, output, name)
	}
	scheme := runtime.NewScheme()
	if err := seaplanev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store, closeStore, err := fleetdb.NewClient(fleetdb.Options{Scheme: scheme, DSN: db.ConnStr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeStore)
	authzSeedClusters(t, store)
	earlier, fault := authzMarkLateFault(t, store)
	earlierID, faultID := string(earlier.UID), string(fault.UID)
	// Exercise the deployed-test policy with exact identities from the disposable store.
	policy, err := os.ReadFile("../e2e-platform-monitoring/testdata/authz-error.cedar")
	if err != nil {
		t.Fatal(err)
	}
	content := strings.NewReplacer("@@ACCOUNT_ID@@", authzAccount, "@@REGION@@", authzRegion,
		"@@RESOURCE_UID@@", faultID, "@@RUN_ID@@", authzFaultValue).Replace(string(policy))
	encodedPolicy, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	bundle = bytes.ReplaceAll(bundle, []byte(`"@@RUNTIME_FAULT_POLICY@@"`), encodedPolicy)
	bundle = bytes.ReplaceAll(bundle, []byte("@@BLUE_CLUSTER_UID@@"), []byte(authzStoredClusters[1].id))
	authzWriteFile(t, configPath, bundle)
	process := start(t, "initial")
	t.Cleanup(process.stop)
	if !t.Run("infrastructure", func(t *testing.T) {
		authzWaitReady(t, process)
		log, err := os.ReadFile(process.logPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, endpoint := range []string{process.apiURL, process.healthURL, process.metricsURL} {
			address := strings.TrimPrefix(endpoint, "http://")
			if !bytes.Contains(log, []byte(`"addr":"`+address+`"`)) {
				t.Errorf("listener must honor loopback bind-address environment input: %s; log %s", address, log)
			}
		}
	}) {
		return
	}

	blueOne, blueOneName := authzStoredClusters[1].id, authzStoredClusters[1].name
	blueTwo := authzStoredClusters[3].id
	foreignName := authzStoredClusters[4].name
	alice := authzUser("alice")
	broad := authzUser("broad")
	for _, tc := range []struct {
		name, arn, account, path, outcome string
		status                            int
	}{
		{"ARN-A matching stored label", alice, authzAccount, "/" + blueOneName, "allow", http.StatusOK},
		{"ARN-A different stored label", alice, authzAccount, "/" + authzStoredClusters[0].name, "deny", http.StatusForbidden},
		{"ARN-A missing stored label", alice, authzAccount, "/" + authzStoredClusters[2].name, "deny", http.StatusForbidden},
		{"ARN-B enrolled no grants list", "arn:aws:iam::222222222222:user/bob", authzOtherAccount, "", "deny", http.StatusForbidden},
		{"ARN-B enrolled no grants describe", "arn:aws:iam::222222222222:user/bob", authzOtherAccount, "/" + foreignName, "deny", http.StatusForbidden},
		{"same account no grants", authzUser("bob"), authzAccount, "", "deny", http.StatusForbidden},
		{"unregistered broad policy", "arn:aws:iam::333333333333:user/unregistered", "333333333333", "", "", http.StatusForbidden},
		{"missing account", alice, "", "", "", http.StatusForbidden},
		{"missing ARN", "", authzAccount, "", "", http.StatusForbidden},
		{"missing identity", "", "", "", "", http.StatusForbidden},
		{"identity account mismatch", alice, authzOtherAccount, "", "", http.StatusForbidden},
		{"role session forbid", "arn:aws:sts::111111111111:assumed-role/readers/blocked", authzAccount, "/" + blueOneName, "deny", http.StatusForbidden},
		{"role session forbid list", "arn:aws:sts::111111111111:assumed-role/readers/blocked", authzAccount, "", "deny", http.StatusForbidden},
		{"path role other session permit", "arn:aws:sts::111111111111:assumed-role/readers/allowed", authzAccount, "/" + blueOneName, "allow", http.StatusOK},
		{"global forbid local permit", authzUser("regional"), authzAccount, "/" + blueOneName, "deny", http.StatusForbidden},
		{"global forbid local permit list", authzUser("regional"), authzAccount, "", "deny", http.StatusForbidden},
		{"list-only cannot describe", authzUser("list-only"), authzAccount, "/" + blueOneName, "deny", http.StatusForbidden},
		{"describe-only cannot list", authzUser("describe-only"), authzAccount, "", "deny", http.StatusForbidden},
		{"describe-only permit", authzUser("describe-only"), authzAccount, "/" + blueOneName, "allow", http.StatusOK},
		{"broad foreign get", broad, authzAccount, "/" + foreignName, "", http.StatusNotFound},
		{"broad missing get", broad, authzAccount, "/missing-cluster", "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			operation := "DescribeCluster"
			if tc.path == "" {
				operation = "ListClusters"
			}
			response := authzMeasuredGet(t, process, authzClustersPath+tc.path, tc.arn, tc.account, operation, tc.outcome)
			authzAssertStatus(t, response, tc.status)
			if tc.status == http.StatusOK {
				var cluster public.Cluster
				if err := json.Unmarshal(response.Body, &cluster); err != nil || string(cluster.UID) != blueOne {
					t.Errorf("describe UID = %q, want %q (%v)", cluster.UID, blueOne, err)
				}
			}
		})
	}

	t.Run("runtime policy ignores matching label on another UID", func(t *testing.T) {
		response := authzMeasuredGet(t, process, authzClustersPath+"/"+earlier.Name, authzUser("error-fixture"), authzAccount, "DescribeCluster", "allow")
		authzAssertStatus(t, response, http.StatusOK)
		var cluster public.Cluster
		if err := json.Unmarshal(response.Body, &cluster); err != nil || string(cluster.UID) != earlierID {
			t.Errorf("earlier describe UID = %q, want %q (%v)", cluster.UID, earlierID, err)
		}
		response = authzMeasuredGet(t, process, authzClustersPath+"?limit=1", broad, authzAccount, "ListClusters", "allow")
		page := authzReadList(t, response, 4, 1, 0)
		if got := authzClusterIDs(page); !slices.Equal(got, []string{earlierID}) || earlierID == faultID {
			t.Fatalf("first HTTP page = %v, want earlier allowed candidate %s, not fault %s", got, earlierID, faultID)
		}
	})
	for _, tc := range []struct{ name, path, operation string }{
		{"runtime policy describe failure", "/" + fault.Name, "DescribeCluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeLog, err := os.ReadFile(process.logPath)
			if err != nil {
				t.Fatal(err)
			}
			before := authzMetrics(t, process)
			apiClient := NewAPIClient(process.apiURL)
			apiClient.CallerARN = authzUser("error-fixture")
			response, err := apiClient.Get(authzClustersPath+tc.path, authzAccount)
			if err != nil {
				t.Fatal("unsigned local request: ", err)
			}
			authzMetricDelta(t, before, authzMetrics(t, process), tc.operation, "error", "evaluation")
			authzAssertStatus(t, response, http.StatusInternalServerError)
			var status metav1.Status
			if err := json.Unmarshal(response.Body, &status); err != nil || status.Reason != metav1.StatusReasonInternalError || status.Message != "AUTHZ-FAILED-001: Authorization failed" || status.Details != nil {
				t.Errorf("runtime failure must contain only safe typed Status details: %s (%v)", response.Body, err)
			}
			if response.Headers.Get("Content-Type") != "application/json" {
				t.Errorf("failure Content-Type = %q", response.Headers.Get("Content-Type"))
			}
			log, err := os.ReadFile(process.logPath)
			if err != nil || len(log) < len(beforeLog) {
				t.Fatalf("read request failure log: %v", err)
			}
			bundleHash := fmt.Sprintf("%x", sha256.Sum256(bundle))
			authzAssertFailureLog(t, log[len(beforeLog):], tc.operation, bundleHash)
			if bytes.Contains(response.Body, []byte(bundleHash)) {
				t.Error("failure response exposed configuration bundle hash")
			}
			t.Logf("%s HTTP %d body %s", tc.operation, response.StatusCode, response.Body)
		})
	}

	t.Run("list total and pagination", func(t *testing.T) {
		response := authzMeasuredGet(t, process, authzClustersPath+"?limit=100", alice, authzAccount, "ListClusters", "allow")
		full := authzReadList(t, response, 4, 100, 0)
		if got := authzDetailedIDs(full); !slices.Equal(got, authzClusterIDs(full)) {
			t.Errorf("detailed membership = %v, want every item %v", got, authzClusterIDs(full))
		}
		for offset := 0; offset <= 5; offset++ {
			response := authzMeasuredGet(t, process, fmt.Sprintf("%s?limit=1&offset=%d", authzClustersPath, offset), alice, authzAccount, "ListClusters", "allow")
			page := authzReadList(t, response, 4, 1, offset)
			var expected []public.Cluster
			if offset < len(full) {
				expected = full[offset : offset+1]
			}
			if !slices.Equal(authzClusterIDs(page), authzClusterIDs(expected)) {
				t.Errorf("page %d = %v, want observed subsequence %v", offset, authzClusterIDs(page), authzClusterIDs(expected))
			}
		}
	})
	t.Run("list-only returns full objects", func(t *testing.T) {
		response := authzMeasuredGet(t, process, authzClustersPath, authzUser("list-only"), authzAccount, "ListClusters", "allow")
		items := authzReadList(t, response, 4, 50, 0)
		if detailed := authzDetailedIDs(items); len(items) != 4 || len(detailed) != 4 {
			t.Errorf("list-only got %d items, details for %v", len(items), detailed)
		}
	})
	t.Run("account allowlist grants and revokes in seaplane-db", func(t *testing.T) {
		const newcomer = "333333333333"
		send := func(method, path, account, body string) *APIResponse {
			request, err := http.NewRequest(method, process.apiURL+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Amz-Account-Id", account)
			request.Header.Set("X-Amz-Caller-Arn", "arn:aws:iam::"+account+":user/allowlist")
			return authzRawRequest(t, request)
		}
		unregistered := func() bool {
			return bytes.Contains(send(http.MethodGet, authzClustersPath, newcomer, "").Body, []byte("Account is not registered"))
		}
		if !unregistered() {
			t.Fatal("newcomer admitted before enrollment")
		}
		authzAssertStatus(t, send(http.MethodPost, "/api/v0/accounts", newcomer, `{"accountId":"`+newcomer+`"}`), http.StatusForbidden)
		if code := send(http.MethodPost, "/api/v0/accounts", authzAccount, `{"accountId":"`+newcomer+`"}`).StatusCode; code != http.StatusNoContent {
			t.Fatalf("enroll status = %d, want 204", code)
		}
		// Allowlisted accounts get full access to their own resources.
		if code := send(http.MethodGet, authzClustersPath, newcomer, "").StatusCode; code != http.StatusOK {
			t.Fatalf("allowlisted newcomer list status = %d, want 200", code)
		}
		if code := send(http.MethodDelete, "/api/v0/accounts/"+newcomer, authzAccount, "").StatusCode; code != http.StatusNoContent {
			t.Fatalf("revoke status = %d, want 204", code)
		}
		if !unregistered() {
			t.Fatal("revoked newcomer still admitted")
		}
	})
	t.Run("broad list still excludes foreign account", func(t *testing.T) {
		response := authzMeasuredGet(t, process, authzClustersPath, broad, authzAccount, "ListClusters", "allow")
		items := authzReadList(t, response, 4, 50, 0)
		want := authzSorted(authzStoredClusters[0].id, blueOne, authzStoredClusters[2].id, blueTwo)
		if got := authzClusterIDs(items); !slices.Equal(got, want) {
			t.Errorf("account-scoped list = %v, want %v", got, want)
		}
	})
	t.Run("request claims cannot replace stored labels or owner", func(t *testing.T) {
		before := authzMetrics(t, process)
		path := authzClustersPath + "/" + authzStoredClusters[0].name + "?accountId=111111111111&example.com%2Fteam=blue&labels=blue"
		request, err := http.NewRequest(http.MethodGet, process.apiURL+path, strings.NewReader(`{"metadata":{"labels":{"example.com/team":"blue","seaplane.io/account-id":"111111111111"}},"spec":{"accountId":"111111111111"}}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-Amz-Account-Id", authzAccount)
		request.Header.Set("X-Amz-Caller-Arn", alice)
		request.Header.Set("X-Seaplane-Labels", `{"example.com/team":"blue"}`)
		request.Header.Set("X-Seaplane-Account-Id", authzAccount)
		response := authzRawRequest(t, request)
		authzAssertStatus(t, response, http.StatusForbidden)
		authzMetricDelta(t, before, authzMetrics(t, process), "DescribeCluster", "deny", "")
		response = authzMeasuredGet(t, process, authzClustersPath+"/"+foreignName+"?accountId="+authzAccount+"&example.com%2Fteam=blue", broad, authzAccount, "DescribeCluster", "")
		authzAssertStatus(t, response, http.StatusNotFound)
		var stored seaplanev1.Cluster
		if err := store.Get(context.Background(), client.ObjectKey{Namespace: "account-" + authzAccount, Name: authzStoredClusters[0].name}, &stored); err != nil || stored.Annotations[authzTeamLabel] != "red" {
			t.Errorf("request changed stored tags: %+v (%v)", stored.Annotations, err)
		}
	})
	t.Run("public requests leave authorization metrics unchanged", func(t *testing.T) {
		before := authzMetrics(t, process)
		for _, endpoint := range []string{
			process.apiURL + "/api/v0/live", process.apiURL + "/api/v0/ready", process.apiURL + "/api/v0/info",
			process.healthURL + "/healthz", process.healthURL + "/readyz", process.metricsURL + "/metrics",
		} {
			request, err := http.NewRequest(http.MethodGet, endpoint, nil)
			if err != nil {
				t.Fatal(err)
			}
			response := authzRawRequest(t, request)
			if response.StatusCode != http.StatusOK {
				t.Errorf("public %s returned %d", endpoint, response.StatusCode)
			}
		}
		authzMetricDelta(t, before, authzMetrics(t, process), "", "", "")
	})

	t.Run("all resource operations and snapshot CAS", func(t *testing.T) {
		authzResourceOperations(t, process, store)
	})

	t.Run("startup snapshot and restart enrollment", func(t *testing.T) {
		var replacement struct {
			FormatVersion      int               `json:"formatVersion"`
			RegisteredAccounts []string          `json:"registeredAccounts"`
			Policies           []json.RawMessage `json:"policies"`
			Attachments        []json.RawMessage `json:"attachments"`
		}
		if err := json.Unmarshal(bundle, &replacement); err != nil {
			t.Fatal(err)
		}
		replacement.RegisteredAccounts = []string{authzOtherAccount}
		content, err := json.Marshal(replacement)
		if err != nil {
			t.Fatal(err)
		}
		authzWriteFile(t, configPath, content)
		response := authzMeasuredGet(t, process, authzClustersPath+"/"+blueOneName, alice, authzAccount, "DescribeCluster", "allow")
		authzAssertStatus(t, response, http.StatusOK)
		process.stop()
		restarted := start(t, "restarted")
		defer restarted.stop()
		authzWaitReady(t, restarted)
		response = authzMeasuredGet(t, restarted, authzClustersPath+"/"+blueOneName, alice, authzAccount, "", "")
		authzAssertStatus(t, response, http.StatusForbidden)
		authzWriteFile(t, configPath, []byte(`{"formatVersion":1,"registeredAccounts":["111111111111"],"policies":[],"attachments":[`))
		response = authzMeasuredGet(t, restarted, authzClustersPath+"/"+blueOneName, alice, authzAccount, "", "")
		authzAssertStatus(t, response, http.StatusForbidden)
	})
	for _, invalid := range []struct{ name, content string }{
		{"malformed", `{"formatVersion":1,"attachments":[`},
		{"unsupported-version", `{"formatVersion":999,"registeredAccounts":[],"policies":[],"attachments":[]}`},
	} {
		t.Run("invalid startup "+invalid.name, func(t *testing.T) {
			authzWriteFile(t, configPath, []byte(invalid.content))
			invalidProcess := start(t, invalid.name)
			defer invalidProcess.stop()
			authzAssertRejected(t, invalidProcess)
		})
	}
}

func authzUser(name string) string {
	return "arn:aws:iam::" + authzAccount + ":user/" + name
}

func authzWriteFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}

func authzClearAWS(t *testing.T) {
	t.Helper()
	// Remove every credential source, including inherited profiles and container credentials.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "AWS_") {
			t.Setenv(key, "")
		}
	}
	empty := t.TempDir()
	for _, name := range []string{"config", "credentials"} {
		path := filepath.Join(empty, name)
		authzWriteFile(t, path, nil)
		key := "AWS_CONFIG_FILE"
		if name == "credentials" {
			key = "AWS_SHARED_CREDENTIALS_FILE"
		}
		t.Setenv(key, path)
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_REGION", authzRegion)
	t.Setenv("RATE_LIMIT_ENABLED", "false")
}

func authzSeedClusters(t *testing.T, store client.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i, fixture := range authzStoredClusters {
		// A copied fault label must not extend the test policy beyond its target UID.
		tags := map[string]string{authzFaultLabel: authzFaultValue}
		if fixture.team != "" {
			tags[authzTeamLabel] = fixture.team
		}
		cluster := &seaplanev1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "account-" + fixture.account, Name: fixture.name, Labels: map[string]string{authzAccountLabel: fixture.account}, Annotations: tags},
			Spec:       seaplanev1.ClusterSpec{AccountID: fixture.account},
		}
		cluster.Spec.HostedCluster.Platform = hypershiftv1.PlatformSpec{
			Type: hypershiftv1.AWSPlatform,
			AWS:  &hypershiftv1.AWSPlatformSpec{Region: authzRegion},
		}
		// A spec tag must not substitute for the missing or different stored metadata label.
		cluster.Spec.Tags = map[string]string{authzTeamLabel: "blue"}
		if err := store.Create(ctx, cluster); err != nil {
			t.Fatalf("seed %s: %v", fixture.name, err)
		}
		authzStoredClusters[i].id = string(cluster.UID)
	}
}

func authzMarkLateFault(t *testing.T, store client.Client) (earlier, fault *seaplanev1.Cluster) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var candidates seaplanev1.ClusterList
	selector := client.MatchingLabels{authzAccountLabel: authzAccount}
	if err := store.List(ctx, &candidates, selector); err != nil {
		t.Fatal(err)
	}
	if len(candidates.Items) < 2 {
		t.Fatal("late failure needs an earlier account-scoped candidate")
	}
	// Select by observed database order, which a metadata update may also change.
	last := candidates.Items[len(candidates.Items)-1].DeepCopy()
	last.Annotations[authzFaultLabel] = authzFaultValue
	if err := store.Update(ctx, last); err != nil {
		t.Fatal(err)
	}
	return &candidates.Items[0], last
}

func authzStartProcess(t *testing.T, binary, config, dsn, output, name string) *authzProcess {
	t.Helper()
	// Hold all three reservations until port selection is complete to prevent duplicate picks.
	var listeners []net.Listener
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	var ports []string
	for range 3 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		ports = append(ports, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	}
	process := &authzProcess{
		done: make(chan struct{}), logPath: filepath.Join(output, "api-"+name+".log"),
		apiURL: "http://127.0.0.1:" + ports[0], healthURL: "http://127.0.0.1:" + ports[1], metricsURL: "http://127.0.0.1:" + ports[2],
	}
	log, err := os.Create(process.logPath)
	if err != nil {
		t.Fatal(err)
	}
	process.cmd = exec.Command(binary, "serve", "--api-port", ports[0], "--health-port", ports[1], "--metrics-port", ports[2])
	process.cmd.Env = append(os.Environ(),
		"POSTGRES_DSN="+dsn, "AUTHZ_CONFIG_FILE="+config, "AWS_ACCOUNT_ID="+authzAccount,
		"API_BIND_ADDRESS=127.0.0.1", "HEALTH_BIND_ADDRESS=127.0.0.1", "METRICS_BIND_ADDRESS=127.0.0.1",
		"TARGET_GROUP_ARN=arn:aws:elasticloadbalancing:"+authzRegion+":"+authzAccount+":targetgroup/authz-http/fixture",
	)
	process.cmd.Stdout, process.cmd.Stderr = log, log
	for _, listener := range listeners {
		_ = listener.Close()
	}
	if err := process.cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	go func() {
		process.err = process.cmd.Wait()
		_ = log.Close()
		close(process.done)
	}()
	return process
}

func (process *authzProcess) stop() {
	process.stopOnce.Do(func() {
		select {
		case <-process.done:
			return
		default:
		}
		_ = process.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-process.done:
		case <-time.After(authzStopTimeout):
			_ = process.cmd.Process.Kill()
			<-process.done
		}
	})
}

func authzWaitReady(t *testing.T, process *authzProcess) {
	t.Helper()
	deadline := time.Now().Add(authzReadyTimeout)
	httpClient := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		select {
		case <-process.done:
			log, _ := os.ReadFile(process.logPath)
			t.Fatalf("API exited before readiness: %v\n%s", process.err, log)
		default:
		}
		isReady := true
		for _, endpoint := range []string{process.apiURL + "/api/v0/ready", process.healthURL + "/readyz", process.metricsURL + "/metrics"} {
			response, err := httpClient.Get(endpoint)
			if err != nil {
				isReady = false
				continue
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				isReady = false
			}
		}
		if isReady {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log, _ := os.ReadFile(process.logPath)
	t.Fatalf("API readiness exceeded %s\n%s", authzReadyTimeout, log)
}

func authzAssertRejected(t *testing.T, process *authzProcess) {
	t.Helper()
	deadline := time.After(authzReadyTimeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-process.done:
			if process.err == nil {
				t.Error("invalid configuration exited successfully")
			}
			log, _ := os.ReadFile(process.logPath)
			if bytes.Contains(log, []byte("starting API server")) || bytes.Contains(log, []byte("starting health server")) || bytes.Contains(log, []byte("starting metrics server")) {
				t.Errorf("invalid configuration opened a serving listener\n%s", log)
			}
			return
		case <-deadline:
			t.Fatal("invalid configuration did not exit before serving")
		case <-ticker.C:
			for _, endpoint := range []string{process.apiURL, process.healthURL, process.metricsURL} {
				connection, err := net.DialTimeout("tcp", strings.TrimPrefix(endpoint, "http://"), 20*time.Millisecond)
				if err == nil {
					_ = connection.Close()
					t.Fatalf("invalid configuration opened %s", endpoint)
				}
			}
		}
	}
}

func authzAssertFailureLog(t *testing.T, log []byte, operation, bundleHash string) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(log))
	failures := 0
	for decoder.More() {
		var entry struct {
			Message    string `json:"msg"`
			BundleHash string `json:"bundle_hash"`
			Operation  string `json:"operation"`
			Stage      string `json:"stage"`
			Cause      string `json:"cause"`
			Provenance []struct {
				DiagnosticID, PolicyID, BundleHash, AttachmentID string
			}
			Diagnostics []struct {
				Policy   string `json:"policy"`
				Message  string `json:"message"`
				Position struct {
					Line int `json:"line"`
				} `json:"position"`
			}
		}
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal("invalid request log JSON: ", err)
		}
		if entry.Message != "request authorization failed" {
			continue
		}
		failures++
		if entry.BundleHash != bundleHash || entry.Operation != operation || entry.Stage != "evaluation" || entry.Cause != "cedar evaluation diagnostics" || len(entry.Provenance) != 1 || len(entry.Diagnostics) != 1 {
			t.Fatalf("failure log lost evaluation cause or unique provenance: %s", log)
		}
		provenance := entry.Provenance[0]
		diagnostic := entry.Diagnostics[0]
		if provenance.DiagnosticID != "attachment/error-forbid" || provenance.PolicyID != "runtime-fault" || provenance.AttachmentID != "error-forbid" || provenance.BundleHash != bundleHash || diagnostic.Policy != provenance.DiagnosticID || !strings.Contains(diagnostic.Message, "integer overflow") || diagnostic.Position.Line < 1 {
			t.Fatalf("failure log lost actual attachment bundle hash or native overflow diagnostic: %s", log)
		}
	}
	if failures != 1 {
		t.Errorf("request failure logs = %d, want 1; log %s", failures, log)
	}
}

func authzAssertStatus(t *testing.T, response *APIResponse, expected int) {
	t.Helper()
	if response.StatusCode != expected {
		t.Errorf("HTTP status = %d, want %d; body %s", response.StatusCode, expected, response.Body)
	}
	if expected == http.StatusOK {
		return
	}
	var status metav1.Status
	if err := json.Unmarshal(response.Body, &status); err != nil || status.APIVersion != "v1" || status.Kind != "Status" || status.Status != metav1.StatusFailure || int(status.Code) != expected || status.Message == "" {
		t.Errorf("expected structured failure code %d, got %s (%v)", expected, response.Body, err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body, &envelope); err == nil {
		for key := range envelope {
			if !slices.Contains([]string{"apiVersion", "kind", "metadata", "status", "message", "reason", "details", "code"}, key) {
				t.Errorf("failure response contains non-Status field %q", key)
			}
		}
		// Kubernetes Status emits metadata {}, but resource metadata must never leak.
		if metadata, exists := envelope["metadata"]; exists {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(metadata, &fields); err != nil || fields == nil || len(fields) != 0 {
				t.Errorf("failure response contains nonempty or invalid Status metadata")
			}
		}
	}
	for _, secret := range []string{"permit(", "forbid(", "principalARN", "attachment", authzTeamLabel, authzFaultLabel, authzFaultValue, "runtime-fault", "error-forbid", "integer overflow"} {
		if bytes.Contains(response.Body, []byte(secret)) {
			t.Errorf("failure exposed policy or binding detail %q", secret)
		}
	}
}

func authzReadList(t *testing.T, response *APIResponse, total, limit, offset int) []public.Cluster {
	t.Helper()
	authzAssertStatus(t, response, http.StatusOK)
	var list struct {
		Items  []public.Cluster `json:"items"`
		Total  int              `json:"total"`
		Limit  int              `json:"limit"`
		Offset int              `json:"offset"`
	}
	if err := json.Unmarshal(response.Body, &list); err != nil {
		t.Fatal(err)
	}
	if list.Items == nil || list.Total != total || list.Limit != limit || list.Offset != offset {
		t.Errorf("list envelope = %+v, want total=%d limit=%d offset=%d non-null items", list, total, limit, offset)
	}
	return list.Items
}

// authzDetailedIDs returns items with full details; identifier-only items carry no labels.
func authzDetailedIDs(clusters []public.Cluster) []string {
	var ids []string
	for _, cluster := range clusters {
		if cluster.Labels != nil {
			ids = append(ids, string(cluster.UID))
		}
	}
	slices.Sort(ids)
	return ids
}

// authzSorted orders seaplane-db UIDs like the list helpers do.
func authzSorted(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

func authzClusterIDs(clusters []public.Cluster) []string {
	ids := make([]string, 0, len(clusters))
	for _, cluster := range clusters {
		ids = append(ids, string(cluster.UID))
	}
	slices.Sort(ids)
	return ids
}

func authzRawRequest(t *testing.T, request *http.Request) *APIResponse {
	t.Helper()
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return &APIResponse{StatusCode: response.StatusCode, Body: body, Headers: response.Header}
}

func authzMeasuredGet(t *testing.T, process *authzProcess, path, arn, account, operation, outcome string) *APIResponse {
	t.Helper()
	before := authzMetrics(t, process)
	apiClient := NewAPIClient(process.apiURL)
	apiClient.CallerARN = arn
	response, err := apiClient.Get(path, account)
	if err != nil {
		t.Fatal("unsigned local request: ", err)
	}
	authzMetricDelta(t, before, authzMetrics(t, process), operation, outcome, "")
	return response
}

func authzMetrics(t *testing.T, process *authzProcess) map[string]float64 {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, process.metricsURL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := authzRawRequest(t, request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("metrics HTTP status = %d", response.StatusCode)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(response.Body))
	if err != nil {
		t.Fatal("invalid Prometheus exposition: ", err)
	}
	samples := map[string]float64{}
	for name, family := range families {
		if !strings.HasPrefix(name, "authz_") {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			classification := labels["outcome"]
			if name == "authz_failures_total" {
				classification = labels["stage"]
			}
			key := name + "/" + labels["operation"] + "/" + classification
			if name == "authz_duration_seconds" {
				samples[key] = float64(metric.GetHistogram().GetSampleCount())
			} else {
				samples[key] = metric.GetCounter().GetValue()
			}
		}
	}
	return samples
}

func authzMetricDelta(t *testing.T, before, after map[string]float64, operation, outcome, stage string) {
	t.Helper()
	deltas := map[string]float64{}
	for key, value := range after {
		if delta := value - before[key]; delta != 0 {
			deltas[key] = delta
		}
	}
	for key, value := range before {
		if _, exists := after[key]; !exists && value != 0 {
			deltas[key] = -value
		}
	}
	want := map[string]float64{}
	if outcome != "" {
		want["authz_requests_total/"+operation+"/"+outcome] = 1
		want["authz_duration_seconds/"+operation+"/"+outcome] = 1
	}
	if stage != "" {
		want["authz_failures_total/"+operation+"/"+stage] = 1
		t.Logf("request-level authorization metric deltas = %v", deltas)
	}
	if !reflect.DeepEqual(deltas, want) {
		t.Errorf("request-level authorization metric deltas = %v, want %v", deltas, want)
	}
}
