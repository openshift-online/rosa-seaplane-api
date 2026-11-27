package authz

import "testing"

func TestResolvedBindingFailureProvenance(t *testing.T) {
	records := readFixture()
	records[0].Policy.Content = "forbid( secret"
	fixture := resolvedFixture(t, region, records)
	prepared, err := fixture.Prepare(t.Context(), testIdentity(sessionARN), testRequestContext())
	if prepared != nil {
		t.Fatal("invalid stored policy produced prepared inputs")
	}
	failure := checkFailure(t, err, StageBinding)
	if len(failure.Provenance) != 1 {
		t.Fatalf("missing binding provenance: %+v", failure)
	}
	source := failure.Provenance[0]
	if source.PolicyID != "read" || source.AttachmentID != "role-read" || source.PolicyRevision != records[0].Policy.Revision || source.AttachmentRevision != records[0].Attachment.Revision {
		t.Fatalf("lost stored revisions: %+v", source)
	}
}
