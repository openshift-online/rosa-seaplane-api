package render

import (
	"fmt"
	"strings"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

const (
	// imageContentSourceMaxItems matches the HyperShift CRD constraint:
	// +kubebuilder:validation:MaxItems=255 on HostedCluster.spec.imageContentSources.
	imageContentSourceMaxItems = 255
	// ImageContentSourceMaxLength matches the HyperShift CRD constraint:
	// +kubebuilder:validation:MaxLength=255 on ImageContentSource.source and each mirror.
	ImageContentSourceMaxLength = 255
)

// ParseECRSources parses the ECR_MIRROR_SOURCES env var value into a slice of
// source registry paths. Format: comma-separated source registry paths, e.g.:
//
//	"source-registry-a.example.com/org/repo,source-registry-b.example.com/org/repo"
//
// Returns an error if any entry is empty after trimming, if the number of
// entries exceeds imageContentSourceMaxItems, or if any entry exceeds
// imageContentSourceMaxLength characters. These limits mirror the HyperShift
// CRD constraints on HostedCluster.spec.imageContentSources.
func ParseECRSources(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var sources []string
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if len(entry) > ImageContentSourceMaxLength {
			return nil, fmt.Errorf("ECR_MIRROR_SOURCES entry %q exceeds maximum length of %d characters", entry, ImageContentSourceMaxLength)
		}
		sources = append(sources, entry)
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("ECR_MIRROR_SOURCES contains no valid entries")
	}
	if len(sources) > imageContentSourceMaxItems {
		return nil, fmt.Errorf("ECR_MIRROR_SOURCES contains %d entries, exceeding the maximum of %d", len(sources), imageContentSourceMaxItems)
	}
	return sources, nil
}

// ecrImageContentSources returns imageContentSources redirecting each configured
// source to the zero-egress ECR registry. Returns nil when ECR mirror is not enabled.
func ecrImageContentSources(cfg ECRMirrorConfig) []hypershiftv1beta1.ImageContentSource {
	if !cfg.Enabled() {
		return nil
	}

	mirror := fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com/%s",
		cfg.RegistryAccount, cfg.RegistryRegion, cfg.Repository)

	ics := make([]hypershiftv1beta1.ImageContentSource, 0, len(cfg.Sources))
	for _, src := range cfg.Sources {
		ics = append(ics, hypershiftv1beta1.ImageContentSource{
			Source:  src,
			Mirrors: []string{mirror},
		})
	}
	return ics
}
