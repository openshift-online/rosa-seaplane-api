package featuregate

import "sort"

// SeaplaneFeatureGates is the canonical registry of Seaplane feature gates.
var SeaplaneFeatureGates = map[string]FeatureGateInfo{
	"SeaplaneEtcdConfig": {
		Stage:       GA,
		Description: "Allows customers to configure etcd settings",
	},
	"SeaplaneAutoScaling": {
		Stage:       TechPreview,
		Description: "Enables cluster autoscaling configuration",
	},
	"SeaplaneSecretEncryption": {
		Stage:       TechPreview,
		Description: "Allows customers to configure secret encryption",
	},
	"SeaplaneCustomDNS": {
		Stage:       DevPreview,
		Description: "Enables custom DNS configuration for development/testing",
	},
	"SeaplaneKubeletAdvanced": {
		Stage:       TechPreview,
		Description: "Enables advanced kubelet configuration (serializeImagePulls, registryPullQPS, etc.)",
	},
	"SeaplaneMachineConfig": {
		Stage:       TechPreview,
		Description: "Allows customers to request approved kernel parameters via allowlist",
	},
}

func IsGateEnabled(gate string, featureSet FeatureSet) bool {
	info, exists := SeaplaneFeatureGates[gate]
	return exists && featureSet.Includes(info.Stage)
}

func GatesForFeatureSet(featureSet FeatureSet) []string {
	var gates []string
	maxStage := featureSet.MaxStage()
	for gate, info := range SeaplaneFeatureGates {
		if info.Stage <= maxStage {
			gates = append(gates, gate)
		}
	}
	sort.Strings(gates)
	return gates
}
