package authz

import (
	"time"

	cedar "github.com/cedar-policy/cedar-go"
)

// Identity contains only trusted gateway identity values.
type Identity struct {
	AccountID string
	CallerARN string
	// IsAllowlisted grants full access to the account's own resources (temporary allowlist).
	IsAllowlisted bool
}

type Action string

const (
	ListLabelsForResource  Action = "ListLabelsForResource"
	LabelResource          Action = "LabelResource"
	UnlabelResource        Action = "UnlabelResource"
	ListClusters           Action = "ListClusters"
	DescribeCluster        Action = "DescribeCluster"
	CreateCluster          Action = "CreateCluster"
	UpdateCluster          Action = "UpdateCluster"
	UpdateClusterConfig    Action = "UpdateClusterConfig"
	UpdateClusterVersion   Action = "UpdateClusterVersion"
	DeleteCluster          Action = "DeleteCluster"
	ListNodePools          Action = "ListNodePools"
	DescribeNodePool       Action = "DescribeNodePool"
	CreateNodePool         Action = "CreateNodePool"
	UpdateNodePool         Action = "UpdateNodePool"
	ScaleNodePool          Action = "ScaleNodePool"
	UpdateNodePoolVersion  Action = "UpdateNodePoolVersion"
	DeleteNodePool         Action = "DeleteNodePool"
	ListOIDCConfigs        Action = "ListOIDCConfigs"
	DescribeOIDCConfig     Action = "DescribeOIDCConfig"
	CreateOIDCConfig       Action = "CreateOIDCConfig"
	DeleteOIDCConfig       Action = "DeleteOIDCConfig"
	ListDNSReservations    Action = "ListDNSReservations"
	DescribeDNSReservation Action = "DescribeDNSReservation"
	CreateDNSReservation   Action = "CreateDNSReservation"
	DeleteDNSReservation   Action = "DeleteDNSReservation"
)

type ResourceKind string

const (
	Collection     ResourceKind = "Collection"
	Cluster        ResourceKind = "Cluster"
	NodePool       ResourceKind = "NodePool"
	OIDCConfig     ResourceKind = "OIDCConfig"
	DNSReservation ResourceKind = "DNSReservation"
)

type RequestContext struct {
	SourceIP    string
	UserAgent   string
	RequestTime time.Time
	// RequestLabels are the customer labels the request writes, like aws:RequestTag.
	RequestLabels map[string]string
}

type ParentCluster struct {
	ID, AccountID string
	Labels        map[string]string
}

// Resource contains trusted stored identity/labels, or a validated create candidate.
type Resource struct {
	Kind           ResourceKind
	ID, AccountID  string
	Labels         map[string]string
	CollectionKind ResourceKind
	ParentCluster  *ParentCluster
}

// Provenance identifies the policy/attachment revisions or startup bundle behind a decision.
type Provenance struct {
	DiagnosticID       string
	PolicyID           string
	BundleHash         string
	AttachmentID       string
	PolicyRevision     string
	AttachmentRevision string
}

type Decision struct {
	Allowed    bool
	Provenance []Provenance
}

type Stage string

const (
	StageNone             Stage = ""
	StageResolution       Stage = "resolution"
	StageParsing          Stage = "parsing"
	StageBinding          Stage = "binding"
	StageEntityValidation Stage = "entity_validation"
	StageEvaluation       Stage = "evaluation"
	StageResourceLoading  Stage = "resource_loading"
)

// Failure keeps sensitive diagnostic material separate from the safe error text.
type Failure struct {
	Stage       Stage
	Action      Action
	Provenance  []Provenance
	Diagnostics []cedar.DiagnosticError
	Err         error
}

// Error returns a safe message without exposing the internal cause or diagnostics.
func (f *Failure) Error() string { return "authorization failed" }

// Unwrap exposes the internal cause for error inspection.
func (f *Failure) Unwrap() error { return f.Err }
