package cincinnati

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	v1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
)

func TestListReleasesFiltersAndOrdersCincinnatiGraph(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != graphPath || r.URL.Query().Get("channel") != "stable-4.20" ||
			r.URL.Query().Get("arch") != "arm64" || r.Header.Get("Accept") != graphAccept {
			t.Errorf("unexpected request: method=%s url=%s Accept=%q", r.Method, r.URL, r.Header.Get("Accept"))
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"nodes":[
			{"version":"4.20.10","payload":"quay.io/release:10","metadata":{"io.openshift.upgrades.graph.release.channels":"fast-4.20, stable-4.20"}},
			{"version":"4.19.99","payload":"quay.io/release:old","metadata":{"io.openshift.upgrades.graph.release.channels":"stable-4.20"}},
			{"version":"4.20.9","payload":"quay.io/release:9","metadata":{"io.openshift.upgrades.graph.release.channels":"stable-4.20"}},
			{"version":"4.20.2","payload":"quay.io/release:2","metadata":{"io.openshift.upgrades.graph.release.channels":"stable-4.20"}},
			{"version":"4.20.11","payload":"quay.io/release:11","metadata":{"io.openshift.upgrades.graph.release.channels":"stable-4.200"}},
			{"version":"4.20.10","payload":"quay.io/release:10","metadata":{"io.openshift.upgrades.graph.release.channels":"stable-4.20"}},
			{"version":"4.21.0","payload":"quay.io/release:next","metadata":{"io.openshift.upgrades.graph.release.channels":"stable-4.20"}}
		]}`))
	}))
	defer server.Close()

	// The configured URL can be a full graph endpoint, as in the operator flag.
	client, err := NewClient(server.URL+graphPath, "arm64", server.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	releases, err := client.ListReleases(context.Background(), "stable-4.20")
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	want := []v1alpha1.ChannelRelease{
		{Version: "4.20.2", Image: "quay.io/release:2"},
		{Version: "4.20.9", Image: "quay.io/release:9"},
		{Version: "4.20.10", Image: "quay.io/release:10"},
	}
	if !reflect.DeepEqual(releases, want) {
		t.Errorf("releases = %#v, want %#v", releases, want)
	}
}

func TestListReleasesIncludesCandidateVersions(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"nodes":[
			{"version":"4.20.0-rc.1","payload":"image-rc","metadata":{"io.openshift.upgrades.graph.release.channels":"candidate-4.20"}},
			{"version":"4.20.0","payload":"image-ga","metadata":{"io.openshift.upgrades.graph.release.channels":"candidate-4.20"}},
			{"version":"4.20.0-ec.2","payload":"image-ec","metadata":{"io.openshift.upgrades.graph.release.channels":"candidate-4.20"}}
		]}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "amd64", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	releases, err := client.ListReleases(context.Background(), "candidate-4.20")
	if err != nil {
		t.Fatal(err)
	}
	want := []v1alpha1.ChannelRelease{
		{Version: "4.20.0-ec.2", Image: "image-ec"},
		{Version: "4.20.0-rc.1", Image: "image-rc"},
		{Version: "4.20.0", Image: "image-ga"},
	}
	if !reflect.DeepEqual(releases, want) {
		t.Errorf("candidate releases = %#v, want %#v", releases, want)
	}
}

func TestListReleasesFailures(t *testing.T) {
	t.Parallel()
	const metadata = `"metadata":{"io.openshift.upgrades.graph.release.channels":"stable-4.20"}`
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "non-2xx", status: http.StatusBadGateway, body: "upstream error", want: "502"},
		{name: "malformed JSON", body: `{"nodes":[`, want: "decode"},
		{name: "missing nodes", body: `{}`, want: "no nodes"},
		{name: "trailing data", body: `{"nodes":[]} {}`, want: "trailing data"},
		{name: "no members", body: `{"nodes":[{"version":"4.20.1","payload":"image","metadata":{"io.openshift.upgrades.graph.release.channels":"stable-4.200"}}]}`, want: "no releases"},
		{name: "only predecessors", body: fmt.Sprintf(`{"nodes":[{"version":"4.19.10","payload":"image",%s}]}`, metadata), want: "no releases"},
		{name: "no image", body: fmt.Sprintf(`{"nodes":[{"version":"4.20.1","payload":"",%s}]}`, metadata), want: "no payload"},
		{name: "invalid version", body: fmt.Sprintf(`{"nodes":[{"version":"4.20.x","payload":"image",%s}]}`, metadata), want: "invalid release version"},
		{name: "conflicting duplicate", body: fmt.Sprintf(`{"nodes":[{"version":"4.20.1","payload":"image-a",%s},{"version":"4.20.1","payload":"image-b",%s}]}`, metadata, metadata), want: "conflicting payload"},
		{name: "body limit", body: `{"nodes":[]}` + strings.Repeat(" ", maxGraphBytes+1), want: "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "amd64", server.Client())
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			_, err = client.ListReleases(context.Background(), "stable-4.20")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ListReleases error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNewClientAndRequestContext(t *testing.T) {
	t.Parallel()
	for _, baseURL := range []string{"", "not-a-url", "ftp://example.com", "https://example.com/graph?channel=override"} {
		if _, err := NewClient(baseURL, "amd64", nil); err == nil {
			t.Errorf("NewClient(%q) accepted invalid endpoint", baseURL)
		}
	}
	if _, err := NewClient("https://example.com", "", nil); err == nil {
		t.Error("NewClient accepted empty architecture")
	}
	client, err := NewClient("https://example.com", "amd64", nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	for _, channel := range []string{"", "stable", "stable-4.20.1", "stable-4.x"} {
		if _, err := client.ListReleases(context.Background(), channel); err == nil {
			t.Errorf("ListReleases(%q) accepted invalid channel", channel)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ListReleases(ctx, "stable-4.20"); err == nil {
		t.Error("ListReleases accepted canceled context")
	}
}
