package authz

// actionResourceKind returns the resource kind for a supported action without inferring it from HTTP paths.
func actionResourceKind(action Action) ResourceKind {
	switch action {
	case CreatePolicy, ListPolicies, DescribePolicy, UpdatePolicy, DeletePolicy:
		return Policy
	case CreateAttachment, ListAttachments, DeleteAttachment:
		return Attachment
	case CreateAttachmentRegional, ListAttachmentsRegional, DeleteAttachmentRegional:
		return RegionalAttachment
	case ListClusters, DescribeCluster, CreateCluster, UpdateCluster, UpdateClusterConfig, UpdateClusterVersion, DeleteCluster:
		return Cluster
	case ListNodePools, DescribeNodePool, CreateNodePool, UpdateNodePool, ScaleNodePool, UpdateNodePoolVersion, DeleteNodePool:
		return NodePool
	case ListOIDCConfigs, DescribeOIDCConfig, CreateOIDCConfig, DeleteOIDCConfig:
		return OIDCConfig
	case ListDNSReservations, DescribeDNSReservation, CreateDNSReservation, DeleteDNSReservation:
		return DNSReservation
	default:
		return ""
	}
}

// listAction reports whether an action targets a collection, including management creates.
func listAction(action Action) bool {
	switch action {
	case ListClusters, ListNodePools, ListOIDCConfigs, ListDNSReservations,
		CreatePolicy, ListPolicies, CreateAttachment, ListAttachments, CreateAttachmentRegional, ListAttachmentsRegional:
		return true
	default:
		return false
	}
}

// collectionName returns the collection segment used in Cedar UIDs for the resource kind.
func collectionName(kind ResourceKind) string {
	switch kind {
	case Policy:
		return "policies"
	case Attachment:
		return "attachments"
	case RegionalAttachment:
		return "regional_attachments"
	case Cluster:
		return "clusters"
	case NodePool:
		return "nodepools"
	case OIDCConfig:
		return "oidc_configs"
	case DNSReservation:
		return "dns_reservations"
	default:
		return ""
	}
}
