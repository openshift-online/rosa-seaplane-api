package cincinnati

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	v1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
)

const (
	graphPath       = "/api/upgrades_info/v1/graph"
	graphAccept     = "application/vnd.redhat.cincinnati.v1+json"
	channelMetadata = "io.openshift.upgrades.graph.release.channels"
	maxGraphBytes   = 16 << 20
	requestTimeout  = 30 * time.Second
)

// Client reads the Cincinnati upgrade graph. It does not select a default release.
type Client struct {
	endpoint   url.URL
	arch       string
	httpClient *http.Client
}

// NewClient accepts either a server base URL or the full Cincinnati graph endpoint.
func NewClient(baseURL, arch string, httpClient *http.Client) (*Client, error) {
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("invalid Cincinnati URL %q", baseURL)
	}
	if strings.TrimSpace(arch) == "" {
		return nil, fmt.Errorf("Cincinnati architecture must not be empty")
	}
	if endpoint.Path == "" || endpoint.Path == "/" {
		endpoint.Path = graphPath
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &Client{endpoint: *endpoint, arch: arch, httpClient: httpClient}, nil
}

type graph struct {
	Nodes []graphNode `json:"nodes"`
}

type graphNode struct {
	Version  string            `json:"version"`
	Payload  string            `json:"payload"`
	Metadata map[string]string `json:"metadata"`
}

func parseNumber(part string) (uint64, error) {
	if part == "" {
		return 0, fmt.Errorf("empty version component")
	}
	for i := range len(part) {
		if part[i] < '0' || part[i] > '9' {
			return 0, fmt.Errorf("non-numeric version component %q", part)
		}
	}
	return strconv.ParseUint(part, 10, 64)
}

func parseVersion(version string) (*semver.Version, error) {
	parsed, err := semver.NewVersion(version)
	if err != nil {
		return nil, fmt.Errorf("invalid release version %q: %w", version, err)
	}
	return parsed, nil
}

func channelMinor(channel string) (uint64, uint64, error) {
	// A channel prefix may itself contain hyphens; the version is its final component.
	split := strings.LastIndexByte(channel, '-')
	if split <= 0 || split == len(channel)-1 {
		return 0, 0, fmt.Errorf("invalid Cincinnati channel %q: expected name-x.y", channel)
	}
	majorPart, minorPart, ok := strings.Cut(channel[split+1:], ".")
	if !ok {
		return 0, 0, fmt.Errorf("invalid Cincinnati channel %q: expected name-x.y", channel)
	}
	major, majorErr := parseNumber(majorPart)
	minor, minorErr := parseNumber(minorPart)
	if majorErr != nil || minorErr != nil {
		return 0, 0, fmt.Errorf("invalid Cincinnati channel %q: expected name-x.y", channel)
	}
	return major, minor, nil
}

func belongsToChannel(channels, channel string) bool {
	for {
		member, remaining, more := strings.Cut(channels, ",")
		if strings.TrimSpace(member) == channel {
			return true
		}
		if !more {
			return false
		}
		channels = remaining
	}
}

// ListReleases returns the exact channel's same-minor releases, sorted by numeric version.
func (c *Client) ListReleases(ctx context.Context, channel string) ([]v1alpha1.ChannelRelease, error) {
	major, minor, err := channelMinor(channel)
	if err != nil {
		return nil, err
	}
	endpoint := c.endpoint
	query := endpoint.Query()
	query.Set("channel", channel)
	query.Set("arch", c.arch)
	endpoint.RawQuery = query.Encode()

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create Cincinnati request: %w", err)
	}
	req.Header.Set("Accept", graphAccept)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Cincinnati graph for %q: %w", channel, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch Cincinnati graph for %q: %s", channel, resp.Status)
	}
	limited := &io.LimitedReader{R: resp.Body, N: maxGraphBytes + 1}
	decoder := json.NewDecoder(limited)
	var feed graph
	decodeErr := decoder.Decode(&feed)
	if limited.N == 0 {
		return nil, fmt.Errorf("Cincinnati graph exceeds %d bytes", maxGraphBytes)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("decode Cincinnati graph: %w", decodeErr)
	}
	if feed.Nodes == nil {
		return nil, fmt.Errorf("Cincinnati graph has no nodes")
	}
	decodeErr = decoder.Decode(new(any))
	if limited.N == 0 {
		return nil, fmt.Errorf("Cincinnati graph exceeds %d bytes", maxGraphBytes)
	}
	if decodeErr != io.EOF {
		return nil, fmt.Errorf("Cincinnati graph has trailing data: %v", decodeErr)
	}

	type sortedRelease struct {
		release v1alpha1.ChannelRelease
		version *semver.Version
	}
	releases := make([]sortedRelease, 0, len(feed.Nodes))
	seen := make(map[string]string, len(feed.Nodes))
	for _, node := range feed.Nodes {
		if node.Version == "" || node.Metadata == nil {
			return nil, fmt.Errorf("Cincinnati graph contains a node without version or metadata")
		}
		if node.Payload == "" {
			return nil, fmt.Errorf("Cincinnati release %q has no payload image", node.Version)
		}
		if !belongsToChannel(node.Metadata[channelMetadata], channel) {
			continue
		}
		version, err := parseVersion(node.Version)
		if err != nil {
			return nil, err
		}
		if version.Major() != major || version.Minor() != minor {
			continue
		}
		if image, ok := seen[node.Version]; ok {
			if image != node.Payload {
				return nil, fmt.Errorf("Cincinnati release %q has conflicting payload images", node.Version)
			}
			continue
		}
		seen[node.Version] = node.Payload
		releases = append(releases, sortedRelease{
			release: v1alpha1.ChannelRelease{Version: node.Version, Image: node.Payload},
			version: version,
		})
	}
	if len(releases) == 0 {
		return nil, fmt.Errorf("Cincinnati graph has no releases for channel %q and minor %d.%d", channel, major, minor)
	}
	slices.SortFunc(releases, func(a, b sortedRelease) int {
		if order := a.version.Compare(b.version); order != 0 {
			return order
		}
		return cmp.Compare(a.release.Version, b.release.Version)
	})
	result := make([]v1alpha1.ChannelRelease, len(releases))
	for i := range releases {
		result[i] = releases[i].release
	}
	return result, nil
}
