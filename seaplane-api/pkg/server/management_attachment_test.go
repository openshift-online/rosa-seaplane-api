package server

import (
	"encoding/json"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

// TestGlobalAttachmentHTTP checks default global scope, response fields, lists, and deletion receipts against real Management.
func TestGlobalAttachmentHTTP(t *testing.T) {
	srv := newManagementServer(t)
	const admin = "arn:aws:sts::999999999999:assumed-role/admin/session"
	created := managementHTTPRequest(srv, admin, "POST", "/api/v0/authz/attachments", `{"policy":{"id":"read","type":"managed"},"principalArn":"arn:aws:iam::999999999999:user/reader"}`, "")
	var attachment struct{ ID, Revision, Scope string }
	if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &attachment) != nil || attachment.Scope != "global" {
		t.Fatalf("global create: %d %s", created.Code, created.Body)
	}
	var fields map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if _, hasRegion := fields["region"]; hasRegion {
		t.Fatalf("global region must be omitted: %s", created.Body)
	}
	for _, query := range []string{"", "?scope=global", "?scope=regional"} {
		listed := managementHTTPRequest(srv, admin, "GET", "/api/v0/authz/attachments"+query, "", "")
		var list struct {
			Kind  string
			Total int
			Items []json.RawMessage
		}
		if listed.Code != 200 || json.Unmarshal(listed.Body.Bytes(), &list) != nil || list.Kind != "AttachmentList" {
			t.Fatalf("list: %d %s", listed.Code, listed.Body)
		}
		want := 1
		if query == "?scope=regional" {
			want = 0
		}
		if list.Total != want || len(list.Items) != want {
			t.Fatalf("wrong scope list: %s", listed.Body)
		}
	}
	deleted := managementHTTPRequest(srv, admin, "DELETE", "/api/v0/authz/attachments/"+attachment.ID+"?scope=global", "", attachment.Revision)
	var receipt authz.DeletionReceipt
	if deleted.Code != 200 || json.Unmarshal(deleted.Body.Bytes(), &receipt) != nil || receipt.DeletedRevision != attachment.Revision || receipt.CommittedRegion != "us-east-1" {
		t.Fatalf("delete: %d %s", deleted.Code, deleted.Body)
	}
}
