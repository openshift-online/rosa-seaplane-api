package alertmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const createdBy = "seaplane-silence-reconciler"

// ReasonInstalling is the lifecycle reason for cluster install silences.
const ReasonInstalling = "installing"

type matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

type silenceStatus struct {
	State string `json:"state"`
}

type gettableSilence struct {
	ID        string        `json:"id"`
	Status    silenceStatus `json:"status"`
	Matchers  []matcher     `json:"matchers"`
	CreatedBy string        `json:"createdBy"`
	Comment   string        `json:"comment"`
}

// ListManagedSilences returns active silences managed by the seaplane silence
// reconciler, matched by the cluster's namespace and name. namespace must be the
// Cluster CR namespace (account-<id>), matching silence.IdentityFromCluster.
func ListManagedSilences(ctx context.Context, baseURL, namespace, clusterName string) ([]gettableSilence, error) {
	values := url.Values{}
	values.Add("filter", fmt.Sprintf(`namespace=%q`, namespace))
	values.Add("filter", fmt.Sprintf(`name=%q`, clusterName))
	endpoint := strings.TrimRight(baseURL, "/") + "/api/v2/silences?" + values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build list request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list silences: request failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list silences: %s", resp.Status)
	}

	var silences []gettableSilence
	if err := json.NewDecoder(resp.Body).Decode(&silences); err != nil {
		return nil, fmt.Errorf("decode silences: %w", err)
	}

	var managed []gettableSilence
	for _, s := range silences {
		if s.CreatedBy == createdBy && s.Status.State == "active" {
			managed = append(managed, s)
		}
	}
	return managed, nil
}

// HasInstallingSilence reports whether any managed silence is for cluster install.
func HasInstallingSilence(silences []gettableSilence) bool {
	want := "lifecycle=" + ReasonInstalling
	for _, s := range silences {
		if strings.Contains(s.Comment, want) {
			return true
		}
	}
	return false
}
