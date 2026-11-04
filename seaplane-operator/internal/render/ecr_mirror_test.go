package render

import (
	"testing"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

func findHostedCluster(t *testing.T, resources []Resource) *hypershiftv1beta1.HostedCluster {
	t.Helper()
	for _, r := range resources {
		if r.Resource == "hostedclusters" {
			return r.Object.(*hypershiftv1beta1.HostedCluster)
		}
	}
	t.Fatal("no hostedcluster resource found")
	return nil
}

func TestParseECRSources(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{"empty string", "", nil, false},
		{"single source", "source-a.example.com/org/repo", []string{"source-a.example.com/org/repo"}, false},
		{"multiple sources", "source-a.example.com/org/repo,source-b.example.com/org/repo", []string{"source-a.example.com/org/repo", "source-b.example.com/org/repo"}, false},
		{"trims whitespace", " source-a.example.com/org/repo , source-b.example.com/org/repo ", []string{"source-a.example.com/org/repo", "source-b.example.com/org/repo"}, false},
		{"only commas", ",,,", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseECRSources(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseECRSources(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d sources, want %d", len(got), len(tt.want))
			}
			for i, s := range tt.want {
				if got[i] != s {
					t.Errorf("source %d: got %q, want %q", i, got[i], s)
				}
			}
		})
	}
}

func TestECRMirrorConfig_Enabled(t *testing.T) {
	sources := []string{"source-a.example.com/org/repo"}
	tests := []struct {
		name string
		cfg  ECRMirrorConfig
		want bool
	}{
		{"all fields set", ECRMirrorConfig{RegistryAccount: "123456789012", RegistryRegion: "us-east-1", Repository: "example-repo", Sources: sources}, true},
		{"fully empty", ECRMirrorConfig{}, false},
		{"account missing", ECRMirrorConfig{RegistryRegion: "us-east-1", Repository: "example-repo", Sources: sources}, false},
		{"region missing", ECRMirrorConfig{RegistryAccount: "123456789012", Repository: "example-repo", Sources: sources}, false},
		{"repository missing", ECRMirrorConfig{RegistryAccount: "123456789012", RegistryRegion: "us-east-1", Sources: sources}, false},
		{"sources empty", ECRMirrorConfig{RegistryAccount: "123456789012", RegistryRegion: "us-east-1", Repository: "example-repo"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Enabled(); got != tt.want {
				t.Errorf("Enabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestECRImageContentSources(t *testing.T) {
	sources := []string{"source-a.example.com/org/repo", "source-b.example.com/org/repo"}
	cfg := ECRMirrorConfig{
		RegistryAccount: "123456789012",
		RegistryRegion:  "us-east-1",
		Repository:      "example-org/ocp-mirror",
		Sources:         sources,
	}
	wantMirror := "123456789012.dkr.ecr.us-east-1.amazonaws.com/example-org/ocp-mirror"

	tests := []struct {
		name        string
		cfg         ECRMirrorConfig
		wantNil     bool
		wantMirror  string
		wantSources []string
	}{
		{
			name:        "enabled returns all configured sources",
			cfg:         cfg,
			wantMirror:  wantMirror,
			wantSources: sources,
		},
		{
			name:    "fully empty config returns nil",
			cfg:     ECRMirrorConfig{},
			wantNil: true,
		},
		{
			name:    "sources empty returns nil",
			cfg:     ECRMirrorConfig{RegistryAccount: "123456789012", RegistryRegion: "us-east-1", Repository: "example-repo"},
			wantNil: true,
		},
		{
			name:    "account missing returns nil",
			cfg:     ECRMirrorConfig{RegistryRegion: "us-east-1", Repository: "example-repo", Sources: sources},
			wantNil: true,
		},
		{
			name:    "region missing returns nil",
			cfg:     ECRMirrorConfig{RegistryAccount: "123456789012", Repository: "example-repo", Sources: sources},
			wantNil: true,
		},
		{
			name:    "repository missing returns nil",
			cfg:     ECRMirrorConfig{RegistryAccount: "123456789012", RegistryRegion: "us-east-1", Sources: sources},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ecrImageContentSources(tt.cfg)
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			if len(got) != len(tt.wantSources) {
				t.Fatalf("got %d entries, want %d", len(got), len(tt.wantSources))
			}
			for i, ics := range got {
				if ics.Source != tt.wantSources[i] {
					t.Errorf("entry %d source: got %q, want %q", i, ics.Source, tt.wantSources[i])
				}
				if len(ics.Mirrors) != 1 || ics.Mirrors[0] != tt.wantMirror {
					t.Errorf("entry %d mirrors: got %v, want [%q]", i, ics.Mirrors, tt.wantMirror)
				}
			}
		})
	}
}

func TestClusterResources_ECRMirrorSetsImageContentSources(t *testing.T) {
	cfg := ECRMirrorConfig{
		RegistryAccount: "123456789012",
		RegistryRegion:  "us-east-1",
		Repository:      "example-org/ocp-mirror",
		Sources:         []string{"source-a.example.com/org/repo", "source-b.example.com/org/repo"},
	}
	wantMirror := "123456789012.dkr.ecr.us-east-1.amazonaws.com/example-org/ocp-mirror"

	resources, err := ClusterResources(testCluster(), false, "f7a3.0.example.com", "", cfg)
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}

	hc := findHostedCluster(t, resources)
	if len(hc.Spec.ImageContentSources) == 0 {
		t.Fatal("expected ImageContentSources to be set when ECR mirror is enabled")
	}
	for _, ics := range hc.Spec.ImageContentSources {
		if len(ics.Mirrors) != 1 || ics.Mirrors[0] != wantMirror {
			t.Errorf("unexpected mirror %v for source %q", ics.Mirrors, ics.Source)
		}
	}
}

func TestClusterResources_ECRMirrorDisabledLeavesImageContentSourcesUnset(t *testing.T) {
	resources, err := ClusterResources(testCluster(), false, "f7a3.0.example.com", "", ECRMirrorConfig{})
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}

	hc := findHostedCluster(t, resources)
	if len(hc.Spec.ImageContentSources) != 0 {
		t.Errorf("expected ImageContentSources to be empty when ECR mirror is disabled, got %v", hc.Spec.ImageContentSources)
	}
}
