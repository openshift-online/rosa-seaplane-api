package validation

import "testing"

func TestValidateReleaseImage(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		wantErr bool
	}{
		{name: "empty", wantErr: true},
		{name: "whitespace", image: "   ", wantErr: true},
		{name: "one character", image: "a", wantErr: true},
		{name: "pullspec", image: "quay.io/openshift-release-dev/ocp-release:5.0.0-rc.5-multi"},
		{name: "digest", image: "quay.io/openshift-release-dev/ocp-release@sha256:0123456789abcdef"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateReleaseImage(tt.image)
			if tt.wantErr && len(errs) == 0 {
				t.Fatal("expected validation error, got nil")
			}
			if !tt.wantErr && len(errs) != 0 {
				t.Fatalf("expected no validation error, got %v", errs)
			}
			if tt.wantErr && errs[0].Field != releaseImageField {
				t.Fatalf("error field = %q, want %q", errs[0].Field, releaseImageField)
			}
		})
	}
}
