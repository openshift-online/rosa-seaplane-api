package authz

// Management actions require explicit grants separate from resource action groups.
const (
	CreatePolicy             Action = "CreatePolicy"
	ListPolicies             Action = "ListPolicies"
	DescribePolicy           Action = "DescribePolicy"
	UpdatePolicy             Action = "UpdatePolicy"
	DeletePolicy             Action = "DeletePolicy"
	CreateAttachment         Action = "CreateAttachment"
	ListAttachments          Action = "ListAttachments"
	DeleteAttachment         Action = "DeleteAttachment"
	CreateAttachmentRegional Action = "CreateAttachmentRegional"
	ListAttachmentsRegional  Action = "ListAttachmentsRegional"
	DeleteAttachmentRegional Action = "DeleteAttachmentRegional"
)

// Management resource kinds distinguish policies and global or regional attachments.
const (
	Policy             ResourceKind = "Policy"
	Attachment         ResourceKind = "Attachment"
	RegionalAttachment ResourceKind = "RegionalAttachment"
)

// isRegionalAction reports whether an action accepts local regional grants.
func isRegionalAction(action Action) bool {
	switch action {
	case CreateAttachmentRegional, ListAttachmentsRegional, DeleteAttachmentRegional:
		return true
	default:
		return false
	}
}

// isGlobalMutation reports whether an action must run at the account's writer.
func isGlobalMutation(action Action) bool {
	switch action {
	case CreatePolicy, UpdatePolicy, DeletePolicy, CreateAttachment, DeleteAttachment:
		return true
	default:
		return false
	}
}
