package validation

import (
	"encoding/pem"
	"fmt"
	"strings"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
)

func TestValidateImageConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		config  *public.ImageConfiguration
		wantErr bool
		errMsg  string
	}{
		{
			name:    "nil configuration",
			config:  nil,
			wantErr: false,
		},
		{
			name:    "empty configuration",
			config:  &public.ImageConfiguration{},
			wantErr: false,
		},
		{
			name: "valid configuration with all fields",
			config: &public.ImageConfiguration{
				RegistrySources: &public.RegistrySources{
					AllowedRegistries: []string{
						"docker.io",
						"gcr.io",
						"image-registry.openshift-image-registry.svc:5000",
						"quay.io",
						"registry.redhat.io",
						"registry.access.redhat.com",
					},
				},
				AllowedRegistriesForImport: []public.RegistryLocation{
					{DomainName: "docker.io", Insecure: false},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid registry sources",
			config: &public.ImageConfiguration{
				RegistrySources: &public.RegistrySources{
					AllowedRegistries: []string{"docker.io"},
					BlockedRegistries: []string{"gcr.io"},
				},
			},
			wantErr: true,
			errMsg:  "cannot specify both allowedRegistries and blockedRegistries",
		},
		{
			name: "invalid allowed registries for import",
			config: &public.ImageConfiguration{
				AllowedRegistriesForImport: []public.RegistryLocation{
					{DomainName: "docker.io"},
					{DomainName: "docker.io"}, // duplicate
				},
			},
			wantErr: true,
			errMsg:  "duplicate registry location: docker.io",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateImageConfiguration(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateImageConfiguration() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("ValidateImageConfiguration() error = %v, want error containing %q", err, tt.errMsg)
			}
		})
	}
}

func TestValidateRegistrySources(t *testing.T) {
	tests := []struct {
		name    string
		sources *public.RegistrySources
		wantErr bool
		errMsg  string
	}{
		{
			name:    "nil sources",
			sources: nil,
			wantErr: false,
		},
		{
			name:    "empty sources",
			sources: &public.RegistrySources{},
			wantErr: false,
		},
		{
			name: "valid allowed registries with platform registries",
			sources: &public.RegistrySources{
				AllowedRegistries: []string{
					"docker.io",
					"gcr.io",
					"image-registry.openshift-image-registry.svc:5000",
					"quay.io",
					"registry.redhat.io",
					"registry.access.redhat.com",
				},
			},
			wantErr: false,
		},
		{
			name: "allowed registries missing platform registry",
			sources: &public.RegistrySources{
				AllowedRegistries: []string{"docker.io", "gcr.io"},
			},
			wantErr: true,
			errMsg:  "allowedRegistries must include required platform registry",
		},
		{
			name: "valid blocked registries",
			sources: &public.RegistrySources{
				BlockedRegistries: []string{"untrusted.io"},
			},
			wantErr: false,
		},
		{
			name: "valid insecure registries",
			sources: &public.RegistrySources{
				InsecureRegistries: []string{"internal.registry:5000"},
			},
			wantErr: false,
		},
		{
			name: "mutual exclusivity: allowed and blocked",
			sources: &public.RegistrySources{
				AllowedRegistries: []string{"docker.io"},
				BlockedRegistries: []string{"gcr.io"},
			},
			wantErr: true,
			errMsg:  "cannot specify both allowedRegistries and blockedRegistries",
		},
		{
			name: "duplicate in allowed registries",
			sources: &public.RegistrySources{
				AllowedRegistries: []string{
					"docker.io",
					"docker.io",
					"image-registry.openshift-image-registry.svc:5000",
					"quay.io",
					"registry.redhat.io",
					"registry.access.redhat.com",
				},
			},
			wantErr: true,
			errMsg:  "duplicate registry 'docker.io' in allowedRegistries",
		},
		{
			name: "duplicate in blocked registries",
			sources: &public.RegistrySources{
				BlockedRegistries: []string{"bad.io", "bad.io"},
			},
			wantErr: true,
			errMsg:  "duplicate registry 'bad.io' in blockedRegistries",
		},
		{
			name: "duplicate in insecure registries",
			sources: &public.RegistrySources{
				InsecureRegistries: []string{"local.io", "local.io"},
			},
			wantErr: true,
			errMsg:  "duplicate registry 'local.io' in insecureRegistries",
		},
		{
			name: "registry appears in multiple lists",
			sources: &public.RegistrySources{
				AllowedRegistries: []string{
					"docker.io",
					"image-registry.openshift-image-registry.svc:5000",
					"quay.io",
					"registry.redhat.io",
					"registry.access.redhat.com",
				},
				InsecureRegistries: []string{"docker.io"},
			},
			wantErr: true,
			errMsg:  "registry 'docker.io' appears in multiple lists",
		},
		{
			name: "blocked platform registry",
			sources: &public.RegistrySources{
				BlockedRegistries: []string{"quay.io"},
			},
			wantErr: true,
			errMsg:  "cannot block platform registry 'quay.io'",
		},
		{
			name: "insecure and blocked intersection",
			sources: &public.RegistrySources{
				BlockedRegistries:  []string{"bad.io"},
				InsecureRegistries: []string{"bad.io"},
			},
			wantErr: true,
			errMsg:  "insecure registries should not include registries already present in blocked registries",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRegistrySources(tt.sources)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateRegistrySources() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("validateRegistrySources() error = %v, want error containing %q", err, tt.errMsg)
			}
		})
	}
}

func TestValidateAllowedRegistriesForImport(t *testing.T) {
	tests := []struct {
		name      string
		locations []public.RegistryLocation
		wantErr   bool
		errMsg    string
	}{
		{
			name:      "nil locations",
			locations: nil,
			wantErr:   false,
		},
		{
			name:      "empty locations",
			locations: []public.RegistryLocation{},
			wantErr:   false,
		},
		{
			name: "valid single location",
			locations: []public.RegistryLocation{
				{DomainName: "docker.io", Insecure: false},
			},
			wantErr: false,
		},
		{
			name: "valid multiple locations",
			locations: []public.RegistryLocation{
				{DomainName: "docker.io", Insecure: false},
				{DomainName: "gcr.io", Insecure: false},
				{DomainName: "internal.registry:5000", Insecure: true},
			},
			wantErr: false,
		},
		{
			name: "empty domain name",
			locations: []public.RegistryLocation{
				{DomainName: "", Insecure: false},
			},
			wantErr: true,
			errMsg:  "domainName cannot be empty",
		},
		{
			name: "duplicate domain name",
			locations: []public.RegistryLocation{
				{DomainName: "docker.io", Insecure: false},
				{DomainName: "docker.io", Insecure: true},
			},
			wantErr: true,
			errMsg:  "duplicate registry location: docker.io",
		},
		{
			name: "hostname with path",
			locations: []public.RegistryLocation{
				{DomainName: "registry.io/path/to/image", Insecure: false},
			},
			wantErr: true,
			errMsg:  "must be hostname with optional port only",
		},
		{
			name: "hostname with query",
			locations: []public.RegistryLocation{
				{DomainName: "registry.io?query=param", Insecure: false},
			},
			wantErr: true,
			errMsg:  "must be hostname with optional port only",
		},
		{
			name: "hostname with fragment",
			locations: []public.RegistryLocation{
				{DomainName: "registry.io#fragment", Insecure: false},
			},
			wantErr: true,
			errMsg:  "must be hostname with optional port only",
		},
		{
			name: "hostname with user info",
			locations: []public.RegistryLocation{
				{DomainName: "user@registry.io", Insecure: false},
			},
			wantErr: true,
			errMsg:  "must be hostname with optional port only",
		},
		{
			name: "hostname with scheme",
			locations: []public.RegistryLocation{
				{DomainName: "https://registry.io", Insecure: false},
			},
			wantErr: true,
			errMsg:  "must be hostname with optional port only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAllowedRegistriesForImport(tt.locations)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateAllowedRegistriesForImport() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("validateAllowedRegistriesForImport() error = %v, want error containing %q", err, tt.errMsg)
			}
		})
	}
}

func TestValidateAdditionalTrustedCa(t *testing.T) {
	validCA := testPEMCertificate(t, true)
	invalidPEM := "not a PEM certificate"
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("key")}))

	tests := []struct {
		name    string
		certs   map[string]string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "nil certificates",
			certs:   nil,
			wantErr: false,
		},
		{
			name:    "empty certificates",
			certs:   map[string]string{},
			wantErr: false,
		},
		{
			name: "valid certificate",
			certs: map[string]string{
				"registry.example.com": validCA,
			},
			wantErr: false,
		},
		{
			name: "valid multiple certificates",
			certs: map[string]string{
				"registry1.example.com": validCA,
				"registry2.example.com": validCA,
			},
			wantErr: false,
		},
		{
			name: "empty hostname",
			certs: map[string]string{
				"": validCA,
			},
			wantErr: true,
			errMsg:  "hostname cannot be empty",
		},
		{
			name: "empty certificate",
			certs: map[string]string{
				"registry.example.com": "",
			},
			wantErr: true,
			errMsg:  "certificate for 'registry.example.com' cannot be empty",
		},
		{
			name: "invalid PEM format",
			certs: map[string]string{
				"registry.example.com": invalidPEM,
			},
			wantErr: true,
			errMsg:  "must contain only PEM-encoded CA certificates",
		},
		{
			name: "non-certificate PEM type",
			certs: map[string]string{
				"registry.example.com": privateKey,
			},
			wantErr: true,
			errMsg:  "must contain only PEM-encoded CA certificates",
		},
		{
			name: "exceeds ConfigMap size limit",
			certs: func() map[string]string {
				// Create a map that exceeds 1 MiB (1,048,576 bytes)
				// Use a large certificate bundle repeated many times
				largeCert := validCA
				certs := make(map[string]string)
				totalSize := 0
				i := 0
				// Add certificates until we exceed the limit
				for totalSize < 1048577 { // Just over 1 MiB
					hostname := fmt.Sprintf("registry%d.example.com", i)
					certs[hostname] = largeCert
					totalSize += len(hostname) + len(largeCert)
					i++
				}
				return certs
			}(),
			wantErr: true,
			errMsg:  "exceeds Kubernetes ConfigMap limit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAdditionalTrustedCa(tt.certs)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateAdditionalTrustedCa() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("validateAdditionalTrustedCa() error = %v, want error containing %q", err, tt.errMsg)
			}
		})
	}
}

func TestRegistryCertificateBundles(t *testing.T) {
	validCA := testPEMCertificate(t, true)
	nonCA := testPEMCertificate(t, false)

	tests := []struct {
		name     string
		bundle   string
		hostname string
		wantErr  bool
		errMsg   string
	}{
		{
			name:     "valid single certificate",
			bundle:   validCA,
			hostname: "registry.example.com",
			wantErr:  false,
		},
		{
			name:     "valid multiple certificates",
			bundle:   validCA + "\n" + validCA,
			hostname: "registry.example.com",
			wantErr:  false,
		},
		{
			name:     "not PEM format",
			bundle:   "not a certificate",
			hostname: "registry.example.com",
			wantErr:  true,
			errMsg:   "must contain only PEM-encoded CA certificates",
		},
		{
			name:     "empty bundle",
			bundle:   "",
			hostname: "registry.example.com",
			wantErr:  true,
			errMsg:   "cannot be empty",
		},
		{
			name:     "whitespace only",
			bundle:   "   \n\n   ",
			hostname: "registry.example.com",
			wantErr:  true,
			errMsg:   "must contain at least one PEM-encoded CA certificate",
		},
		{
			name:     "non-CA certificate",
			bundle:   nonCA,
			hostname: "registry.example.com",
			wantErr:  true,
			errMsg:   "must contain only valid PEM-encoded CA certificates",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRegistryAdditionalTrustedCA(map[string]string{tt.hostname: tt.bundle})
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateRegistryAdditionalTrustedCA() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("ValidateRegistryAdditionalTrustedCA() error = %v, want error containing %q", err, tt.errMsg)
			}
		})
	}
}

func TestValidateRegistryAdditionalTrustedCA(t *testing.T) {
	validCA := testPEMCertificate(t, true)

	tests := []struct {
		name    string
		certs   map[string]string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "nil certificates",
			certs:   nil,
			wantErr: false,
		},
		{
			name:    "empty certificates",
			certs:   map[string]string{},
			wantErr: false,
		},
		{
			name: "valid certificate",
			certs: map[string]string{
				"registry.example.com": validCA,
			},
			wantErr: false,
		},
		{
			name: "valid multiple certificates",
			certs: map[string]string{
				"registry1.example.com": validCA,
				"registry2.example.com": validCA,
			},
			wantErr: false,
		},
		{
			name: "empty hostname",
			certs: map[string]string{
				"": validCA,
			},
			wantErr: true,
			errMsg:  "hostname cannot be empty",
		},
		{
			name: "empty certificate",
			certs: map[string]string{
				"registry.example.com": "",
			},
			wantErr: true,
			errMsg:  "certificate for 'registry.example.com' cannot be empty",
		},
		{
			name: "invalid PEM format",
			certs: map[string]string{
				"registry.example.com": "not a PEM certificate",
			},
			wantErr: true,
			errMsg:  "must contain only PEM-encoded CA certificates",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRegistryAdditionalTrustedCA(tt.certs)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateRegistryAdditionalTrustedCA() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("ValidateRegistryAdditionalTrustedCA() error = %v, want error containing %q", err, tt.errMsg)
			}
		})
	}
}

// testPEMCertificate is defined in additional_trust_bundle_test.go
