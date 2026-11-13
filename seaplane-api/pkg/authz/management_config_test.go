package authz

import (
	"testing"
)

// TestManagementActionGroups keeps management grants out of resource action groups.
func TestManagementActionGroups(t *testing.T) {
	model, _, err := loadSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CreatePolicy", "ListPolicies", "DescribePolicy", "UpdatePolicy", "DeletePolicy", "CreateAttachment", "ListAttachments", "DeleteAttachment", "CreateAttachmentRegional", "ListAttachmentsRegional", "DeleteAttachmentRegional"} {
		action := model.Actions[entityUID("Action", name)].Entity
		for _, group := range []string{"AllActions", "ReadOnly"} {
			if action.Parents.Contains(entityUID("Action", group)) {
				t.Fatalf("%s inherits %s", name, group)
			}
		}
	}
}
