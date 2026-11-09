package validation

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
)

const (
	MaxRegistryEntries    = 256
	MaxConfigMapDataBytes = 1048576 // 1 MiB - Kubernetes ConfigMap data limit
)

var (
	// Platform registries that cannot be blocked
	platformAllowlist = []string{
		"image-registry.openshift-image-registry.svc:5000",
		"quay.io",
		"registry.redhat.io",
		"registry.access.redhat.com",
	}
)

// ValidateImageConfiguration validates the image configuration.
func ValidateImageConfiguration(config *public.ImageConfiguration) error {
	if config == nil {
		return nil
	}

	if err := validateRegistrySources(config.RegistrySources); err != nil {
		return fmt.Errorf("invalid registrySources: %w", err)
	}

	if err := validateAllowedRegistriesForImport(config.AllowedRegistriesForImport); err != nil {
		return fmt.Errorf("invalid allowedRegistriesForImport: %w", err)
	}

	return nil
}

// ValidateRegistryAdditionalTrustedCA validates registry additional trusted CA certificates.
func ValidateRegistryAdditionalTrustedCA(certs map[string]string) error {
	return validateAdditionalTrustedCa(certs)
}

func validateRegistrySources(sources *public.RegistrySources) error {
	if sources == nil {
		return nil
	}

	// Mutual exclusivity: cannot have both allowed and blocked
	if len(sources.AllowedRegistries) > 0 && len(sources.BlockedRegistries) > 0 {
		return fmt.Errorf("cannot specify both allowedRegistries and blockedRegistries")
	}

	// When using allowlist mode, require all platform registries to be included
	if len(sources.AllowedRegistries) > 0 {
		allowedSet := make(map[string]bool, len(sources.AllowedRegistries))
		for _, reg := range sources.AllowedRegistries {
			allowedSet[reg] = true
		}
		for _, platform := range platformAllowlist {
			if !allowedSet[platform] {
				return fmt.Errorf("allowedRegistries must include required platform registry '%s'", platform)
			}
		}
	}

	// Check for duplicates within each list FIRST (before cross-list checks)
	if err := checkDuplicatesInList(sources.AllowedRegistries, "allowedRegistries"); err != nil {
		return err
	}
	if err := checkDuplicatesInList(sources.BlockedRegistries, "blockedRegistries"); err != nil {
		return err
	}
	if err := checkDuplicatesInList(sources.InsecureRegistries, "insecureRegistries"); err != nil {
		return err
	}

	// Validate platform registries are not blocked
	for _, blocked := range sources.BlockedRegistries {
		for _, platform := range platformAllowlist {
			if blocked == platform || strings.HasSuffix(platform, blocked) {
				return fmt.Errorf("cannot block platform registry '%s'", platform)
			}
		}
	}

	// Check for duplicates across allowed/blocked/insecure (cross-list check)
	allRegistries := make(map[string][]string)
	for _, reg := range sources.AllowedRegistries {
		allRegistries[reg] = append(allRegistries[reg], "allowed")
	}
	for _, reg := range sources.BlockedRegistries {
		allRegistries[reg] = append(allRegistries[reg], "blocked")
	}
	for _, reg := range sources.InsecureRegistries {
		allRegistries[reg] = append(allRegistries[reg], "insecure")
	}

	for reg, lists := range allRegistries {
		if len(lists) > 1 {
			// Special message for insecure+blocked combination
			if len(lists) == 2 {
				hasInsecure := false
				hasBlocked := false
				for _, l := range lists {
					if l == "insecure" {
						hasInsecure = true
					}
					if l == "blocked" {
						hasBlocked = true
					}
				}
				if hasInsecure && hasBlocked {
					return fmt.Errorf("insecure registries should not include registries already present in blocked registries, found duplicated '%s'", reg)
				}
			}
			return fmt.Errorf("registry '%s' appears in multiple lists: %v", reg, lists)
		}
	}

	return nil
}

func checkDuplicatesInList(registries []string, listName string) error {
	seen := make(map[string]bool)
	for _, reg := range registries {
		if seen[reg] {
			return fmt.Errorf("duplicate registry '%s' in %s", reg, listName)
		}
		seen[reg] = true
	}
	return nil
}

func validateAllowedRegistriesForImport(locations []public.RegistryLocation) error {
	seen := make(map[string]bool)
	for _, loc := range locations {
		if loc.DomainName == "" {
			return fmt.Errorf("domainName cannot be empty")
		}
		if seen[loc.DomainName] {
			return fmt.Errorf("duplicate registry location: %s", loc.DomainName)
		}
		seen[loc.DomainName] = true

		// Validate hostname: must be host-only (with optional port), no path/query/fragment/user
		u, err := url.Parse("https://" + loc.DomainName)
		if err != nil {
			return fmt.Errorf("invalid registry hostname '%s': %w", loc.DomainName, err)
		}
		if u.Host != loc.DomainName || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return fmt.Errorf("invalid registry hostname '%s': must be hostname with optional port only (no path, query, fragment, or user info)", loc.DomainName)
		}
	}
	return nil
}

func validateAdditionalTrustedCa(certs map[string]string) error {
	// Calculate total size of ConfigMap data (keys + values)
	totalSize := 0
	for hostname, pemCert := range certs {
		if hostname == "" {
			return fmt.Errorf("hostname cannot be empty in additionalTrustedCa")
		}
		if pemCert == "" {
			return fmt.Errorf("certificate for '%s' cannot be empty", hostname)
		}

		// Validate PEM format and that they are valid CA certificates
		if err := validatePEMTrustBundle(pemCert); err != nil {
			return fmt.Errorf("certificate for '%s' %w", hostname, err)
		}

		// Accumulate size (hostname key + certificate value)
		totalSize += len(hostname) + len(pemCert)
	}

	// Check against Kubernetes ConfigMap 1 MiB limit
	if totalSize > MaxConfigMapDataBytes {
		return fmt.Errorf("total size of registry CA certificates (%d bytes) exceeds Kubernetes ConfigMap limit of %d bytes (1 MiB)", totalSize, MaxConfigMapDataBytes)
	}

	return nil
}
