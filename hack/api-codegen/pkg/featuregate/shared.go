package featuregate

import sharedfeaturegate "github.com/openshift-online/rosa-seaplane-api/featuregate"

type FeatureStage = sharedfeaturegate.FeatureStage
type FeatureGateInfo = sharedfeaturegate.FeatureGateInfo
type FeatureSet = sharedfeaturegate.FeatureSet

const (
	GA                   = sharedfeaturegate.GA
	TechPreview          = sharedfeaturegate.TechPreview
	DevPreview           = sharedfeaturegate.DevPreview
	Default              = sharedfeaturegate.Default
	TechPreviewNoUpgrade = sharedfeaturegate.TechPreviewNoUpgrade
	DevPreviewNoUpgrade  = sharedfeaturegate.DevPreviewNoUpgrade
)

var SeaplaneFeatureGates = sharedfeaturegate.SeaplaneFeatureGates

func IsGateEnabled(gate string, featureSet FeatureSet) bool {
	return sharedfeaturegate.IsGateEnabled(gate, featureSet)
}

func GatesForFeatureSet(featureSet FeatureSet) []string {
	return sharedfeaturegate.GatesForFeatureSet(featureSet)
}
