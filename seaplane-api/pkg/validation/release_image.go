package validation

import "regexp"

const releaseImageField = "spec.hostedCluster.release.image"

// HyperShift applies the same rule to HostedCluster.spec.release.image.
var releaseImagePattern = regexp.MustCompile(`^\w+\S+$`)

// ValidateReleaseImage requires a non-empty OCP release image pullspec accepted
// by HyperShift's HostedCluster validation.
func ValidateReleaseImage(image string) ValidationErrors {
	if image == "" {
		return ValidationErrors{&ValidationError{
			Field:  releaseImageField,
			Reason: "is required and must be a full OCP release image pullspec",
		}}
	}
	if !releaseImagePattern.MatchString(image) {
		return ValidationErrors{&ValidationError{
			Field:  releaseImageField,
			Reason: "must be a valid image pullspec with no whitespace",
		}}
	}
	return nil
}
