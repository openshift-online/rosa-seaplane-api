package featuregate

// FeatureStage represents the maturity stage of a feature gate.
type FeatureStage int

const (
	GA FeatureStage = iota
	TechPreview
	DevPreview
)

func (s FeatureStage) String() string {
	switch s {
	case GA:
		return "GA"
	case TechPreview:
		return "TechPreview"
	case DevPreview:
		return "DevPreview"
	default:
		return "Unknown"
	}
}

type FeatureGateInfo struct {
	Stage       FeatureStage
	Description string
}

type FeatureSet string

const (
	Default              FeatureSet = "Default"
	TechPreviewNoUpgrade FeatureSet = "TechPreviewNoUpgrade"
	DevPreviewNoUpgrade  FeatureSet = "DevPreviewNoUpgrade"
)

func (fs FeatureSet) MaxStage() FeatureStage {
	switch fs {
	case TechPreviewNoUpgrade:
		return TechPreview
	case DevPreviewNoUpgrade:
		return DevPreview
	default:
		return GA
	}
}

func (fs FeatureSet) Includes(stage FeatureStage) bool {
	return stage <= fs.MaxStage()
}
