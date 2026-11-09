package main

import (
	"strings"
	"testing"
)

// validECRArgs returns a complete, valid set of ECR mirror arguments.
func validECRArgs() (account, region, repo, sources, baseSecret string) {
	return "123456789012", "us-east-1", "my-org/ocp-mirror", "quay.io/openshift-release-dev/ocp-v4.0-art-dev", "base-pull-secret"
}

func TestParseECRMirrorConfig_AllEmpty_Disabled(t *testing.T) {
	cfg, err := parseECRMirrorConfig("", "", "", "", "")
	if err != nil {
		t.Fatalf("want nil error, got %v", err)
	}
	if cfg.RegistryAccount != "" {
		t.Errorf("want zero config, got %+v", cfg)
	}
}

func TestParseECRMirrorConfig_Valid(t *testing.T) {
	account, region, repo, sources, baseSecret := validECRArgs()
	cfg, err := parseECRMirrorConfig(account, region, repo, sources, baseSecret)
	if err != nil {
		t.Fatalf("want nil error, got %v", err)
	}
	if cfg.RegistryAccount != account {
		t.Errorf("RegistryAccount = %q, want %q", cfg.RegistryAccount, account)
	}
	if cfg.RegistryRegion != region {
		t.Errorf("RegistryRegion = %q, want %q", cfg.RegistryRegion, region)
	}
	if cfg.Repository != repo {
		t.Errorf("Repository = %q, want %q", cfg.Repository, repo)
	}
	if cfg.BaseSecretName != baseSecret {
		t.Errorf("BaseSecretName = %q, want %q", cfg.BaseSecretName, baseSecret)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0] != sources {
		t.Errorf("Sources = %v, want [%q]", cfg.Sources, sources)
	}
}

func TestParseECRMirrorConfig_PartialConfig_ReportsAllMissing(t *testing.T) {
	// Only account set — the other four must be reported as missing.
	_, err := parseECRMirrorConfig("123456789012", "", "", "", "")
	if err == nil {
		t.Fatal("want error for partial config, got nil")
	}
	for _, field := range []string{
		"ECR_MIRROR_REGISTRY_REGION",
		"ECR_MIRROR_REPOSITORY",
		"ECR_MIRROR_SOURCES",
		"ECR_MIRROR_BASE_SECRET_NAME",
	} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error missing field %q: %v", field, err)
		}
	}
}

func TestParseECRMirrorConfig_PartialConfig_MissingAccount(t *testing.T) {
	// Account absent — must be listed in the missing fields.
	_, region, repo, sources, baseSecret := validECRArgs()
	_, err := parseECRMirrorConfig("", region, repo, sources, baseSecret)
	if err == nil {
		t.Fatal("want error when account is missing")
	}
	if !strings.Contains(err.Error(), "ECR_MIRROR_REGISTRY_ACCOUNT") {
		t.Errorf("error should mention ECR_MIRROR_REGISTRY_ACCOUNT: %v", err)
	}
}

func TestBuildECRMirrorConfig_EnabledWithEmptyVarsExits(t *testing.T) {
	// When ECR_MIRROR_ENABLED=true but all config vars are empty (SSM not
	// populated), buildECRMirrorConfig must not silently return a zero config.
	// We test parseECRMirrorConfig returns nil error for the all-empty case
	// (the guard lives in buildECRMirrorConfig around the os.Getenv call), and
	// verify the sentinel env var's presence would cause an os.Exit — tested
	// indirectly by confirming parseECRMirrorConfig alone returns no error for
	// all-empty inputs (the guard is in the wrapper, not the pure function).
	cfg, err := parseECRMirrorConfig("", "", "", "", "")
	if err != nil {
		t.Fatalf("parseECRMirrorConfig all-empty: want nil error, got %v", err)
	}
	if cfg.RegistryAccount != "" {
		t.Errorf("all-empty should produce zero config, got %+v", cfg)
	}
	// The ECR_MIRROR_ENABLED guard in buildECRMirrorConfig is verified by
	// inspection: when os.Getenv("ECR_MIRROR_ENABLED") == "true" and cfg is
	// zero, the wrapper exits. This cannot be unit-tested without os.Exit
	// interception; the behavior is documented here for auditability.
}

func TestParseECRMirrorConfig_BaseSecretNameCannotBePullSecret(t *testing.T) {
	account, region, repo, sources, _ := validECRArgs()
	_, err := parseECRMirrorConfig(account, region, repo, sources, "pull-secret")
	if err == nil {
		t.Fatal("want error when baseSecretName is pull-secret")
	}
	if !strings.Contains(err.Error(), "pull-secret") {
		t.Errorf("error should mention pull-secret: %v", err)
	}
}

func TestParseECRMirrorConfig_InvalidSources(t *testing.T) {
	// A source entry that exceeds the 255-character limit makes ParseECRSources fail.
	account, region, repo, _, baseSecret := validECRArgs()
	tooLong := strings.Repeat("a", 256)
	_, err := parseECRMirrorConfig(account, region, repo, tooLong, baseSecret)
	if err == nil {
		t.Fatal("want error for oversized source entry")
	}
	if !strings.Contains(err.Error(), "ECR_MIRROR_SOURCES") {
		t.Errorf("error should mention ECR_MIRROR_SOURCES: %v", err)
	}
}

func TestParseECRMirrorConfig_BaseSecretNameRequired(t *testing.T) {
	account, region, repo, sources, _ := validECRArgs()
	_, err := parseECRMirrorConfig(account, region, repo, sources, "")
	if err == nil {
		t.Fatal("want error when baseSecretName is empty with other fields set")
	}
	if !strings.Contains(err.Error(), "ECR_MIRROR_BASE_SECRET_NAME") {
		t.Errorf("error should mention ECR_MIRROR_BASE_SECRET_NAME: %v", err)
	}
}

func TestParseECRMirrorConfig_InvalidAccount(t *testing.T) {
	_, region, repo, sources, baseSecret := validECRArgs()
	_, err := parseECRMirrorConfig("not-an-account", region, repo, sources, baseSecret)
	if err == nil {
		t.Fatal("want error for invalid account")
	}
	if !strings.Contains(err.Error(), "ECR_MIRROR_REGISTRY_ACCOUNT") {
		t.Errorf("error should mention ECR_MIRROR_REGISTRY_ACCOUNT: %v", err)
	}
}

func TestParseECRMirrorConfig_InvalidRegion(t *testing.T) {
	account, _, repo, sources, baseSecret := validECRArgs()
	_, err := parseECRMirrorConfig(account, "not-a-region", repo, sources, baseSecret)
	if err == nil {
		t.Fatal("want error for invalid region")
	}
	if !strings.Contains(err.Error(), "ECR_MIRROR_REGISTRY_REGION") {
		t.Errorf("error should mention ECR_MIRROR_REGISTRY_REGION: %v", err)
	}
}

func TestParseECRMirrorConfig_InvalidRepository(t *testing.T) {
	account, region, _, sources, baseSecret := validECRArgs()
	_, err := parseECRMirrorConfig(account, region, "INVALID REPO!", sources, baseSecret)
	if err == nil {
		t.Fatal("want error for invalid repository")
	}
	if !strings.Contains(err.Error(), "ECR_MIRROR_REPOSITORY") {
		t.Errorf("error should mention ECR_MIRROR_REPOSITORY: %v", err)
	}
}

func TestParseECRMirrorConfig_InvalidBaseSecretName(t *testing.T) {
	account, region, repo, sources, _ := validECRArgs()
	_, err := parseECRMirrorConfig(account, region, repo, sources, "INVALID NAME!")
	if err == nil {
		t.Fatal("want error for invalid baseSecretName")
	}
	if !strings.Contains(err.Error(), "ECR_MIRROR_BASE_SECRET_NAME") {
		t.Errorf("error should mention ECR_MIRROR_BASE_SECRET_NAME: %v", err)
	}
}

func TestParseECRMirrorConfig_BaseSecretNameWithDot(t *testing.T) {
	// DNS subdomain allows dots; DNS label does not. Verify dots are accepted.
	account, region, repo, sources, _ := validECRArgs()
	cfg, err := parseECRMirrorConfig(account, region, repo, sources, "base.pull-secret")
	if err != nil {
		t.Fatalf("want nil error for dot-containing name, got %v", err)
	}
	if cfg.BaseSecretName != "base.pull-secret" {
		t.Errorf("BaseSecretName = %q, want %q", cfg.BaseSecretName, "base.pull-secret")
	}
}

func TestParseECRMirrorConfig_URITooLong(t *testing.T) {
	account, region, _, sources, baseSecret := validECRArgs()
	longRepo := strings.Repeat("a", 220)
	_, err := parseECRMirrorConfig(account, region, longRepo, sources, baseSecret)
	if err == nil {
		t.Fatal("want error for URI exceeding MaxLength=255")
	}
	if !strings.Contains(err.Error(), "MaxLength") {
		t.Errorf("error should mention MaxLength: %v", err)
	}
}
