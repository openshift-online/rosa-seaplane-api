package handlers

import (
	"fmt"
	"log/slog"
	"net/http"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/api"
)

// APIError is an alias for api.APIError so handler code uses the short form.
type APIError = api.APIError

func writeAPIError(w http.ResponseWriter, def APIError, logger *slog.Logger) {
	if err := api.WriteError(w, def); err != nil {
		logger.Error("failed to write error response", "error", err)
	}
}

var ErrResourceConflict = APIError{Code: "RESOURCE-CONFLICT-001", HTTPStatus: http.StatusConflict, Message: "Resource changed; reload and retry"}

// Cluster error codes
var (
	ErrClusterList APIError

	ErrClusterCreateInvalidBody            APIError
	ErrClusterCreateMissingFields          APIError
	ErrClusterCreateFailed                 APIError
	ErrClusterCreateNameCheck              APIError
	ErrClusterCreateOidcConfigLookupFailed APIError
	ErrClusterCreateNameConflict           APIError
	ErrClusterCreateInvalidName            APIError
	ErrClusterCreateInvalidSpec            APIError
	ErrClusterCreateNamespaceMismatch      APIError
	ErrClusterCreateDNSReservationRequired APIError
	ErrClusterCreateDNSReservationNotFound APIError
	ErrClusterCreateDNSReservationNotReady APIError
	ErrClusterCreateDNSReservationInUse    APIError

	ErrClusterCreateOidcConfigRequired APIError
	ErrClusterCreateOidcConfigNotFound APIError
	ErrClusterCreateOidcConfigNotReady APIError
	ErrClusterCreateOidcConfigInUse    APIError

	ErrClusterGetNotFound APIError
	ErrClusterGetFailed   APIError

	ErrClusterUpdateInvalidBody       APIError
	ErrClusterUpdateMissingFields     APIError
	ErrClusterUpdateNotFound          APIError
	ErrClusterUpdateFailed            APIError
	ErrClusterUpdateInvalidSpec       APIError
	ErrClusterUpdateConflict          APIError
	ErrClusterUpdateNamespaceMismatch APIError

	ErrClusterDeleteNotFound APIError
	ErrClusterDeleteFailed   APIError

	ErrClusterStatusNotFound APIError
	ErrClusterStatusFailed   APIError

	ErrClusterValidation APIError
)

// NodePool error codes
var (
	ErrNodePoolList                     APIError
	ErrNodePoolListInvalidClusterUID    APIError
	ErrNodePoolListInvalidLabelSelector APIError

	ErrNodePoolCreateInvalidBody       APIError
	ErrNodePoolCreateMissingFields     APIError
	ErrNodePoolCreateInvalidNamespace  APIError
	ErrNodePoolCreateInvalidName       APIError
	ErrNodePoolCreateInvalidClusterUID APIError
	ErrNodePoolCreateNameConflict      APIError
	ErrNodePoolCreateClusterNotFound   APIError
	ErrNodePoolCreateClusterDeleting   APIError
	ErrNodePoolCreateClusterCheck      APIError
	ErrNodePoolCreateInvalidSpec       APIError
	ErrNodePoolCreateFailed            APIError

	ErrNodePoolGetNotFound APIError
	ErrNodePoolGetFailed   APIError

	ErrNodePoolUpdateInvalidBody      APIError
	ErrNodePoolUpdateMissingFields    APIError
	ErrNodePoolUpdateNotFound         APIError
	ErrNodePoolUpdateFailed           APIError
	ErrNodePoolUpdateInvalidSpec      APIError
	ErrNodePoolUpdateConflict         APIError
	ErrNodePoolUpdateInvalidNamespace APIError

	ErrNodePoolDeleteNotFound APIError
	ErrNodePoolDeleteFailed   APIError

	ErrNodePoolStatusNotFound APIError
	ErrNodePoolStatusFailed   APIError

	ErrNodePoolValidation APIError
)

// Rendered HostedCluster and NodePool error codes
var (
	ErrRenderedHostedClusterList        APIError
	ErrRenderedHostedClusterGetNotFound APIError
	ErrRenderedHostedClusterGetFailed   APIError

	ErrRenderedNodePoolList        APIError
	ErrRenderedNodePoolGetNotFound APIError
	ErrRenderedNodePoolGetFailed   APIError
)

// OidcConfig error codes
var (
	ErrOidcConfigList APIError

	ErrOidcConfigCreateInvalidBody         APIError
	ErrOidcConfigCreateMissingFields       APIError
	ErrOidcConfigCreateInvalidName         APIError
	ErrOidcConfigCreateNameConflict        APIError
	ErrOidcConfigCreateNamespaceMismatch   APIError
	ErrOidcConfigCreateInvalidType         APIError
	ErrOidcConfigCreateInvalidFields       APIError
	ErrOidcConfigCreateDuplicateIssuerUrl  APIError
	ErrOidcConfigCreateInvalidIssuerUrl    APIError
	ErrOidcConfigCreateIssuerNotConfigured APIError
	ErrOidcConfigCreateFailed              APIError

	ErrOidcConfigGetNotFound APIError
	ErrOidcConfigGetFailed   APIError

	ErrOidcConfigDeleteNotFound APIError
	ErrOidcConfigDeleteFailed   APIError
	ErrOidcConfigDeleteInUse    APIError
)

// DNSReservation error codes
var (
	ErrDNSReservationListFailed APIError

	ErrDNSReservationCreateInvalidBody       APIError
	ErrDNSReservationCreateMissingName       APIError
	ErrDNSReservationCreateInvalidName       APIError
	ErrDNSReservationCreateNamespaceMismatch APIError
	ErrDNSReservationCreateNameConflict      APIError
	ErrDNSReservationCreateFailed            APIError

	ErrDNSReservationGetNotFound APIError
	ErrDNSReservationGetFailed   APIError

	ErrDNSReservationDeleteNotFound APIError
	ErrDNSReservationDeleteClaimed  APIError
	ErrDNSReservationDeleteFailed   APIError
)

// Account allowlist error codes
var (
	ErrAccountsForbidden APIError
	ErrAccountsInvalidID APIError
	ErrAccountsFailed    APIError
)

// Info error codes
var ErrInfoRegionalAccountUnavailable APIError

// Router fallback error codes — returned when gorilla/mux finds no matching
// route or method so that the response format matches api.WriteError output.
var (
	ErrRouteNotFound         APIError
	ErrRouteMethodNotAllowed APIError
)

func init() {
	// Cluster — List
	ErrClusterList = APIError{Code: "CLUSTERS-MGMT-LIST-001", HTTPStatus: http.StatusInternalServerError, Message: "Failed to list clusters"}

	// Cluster — Create
	ErrClusterCreateInvalidBody = APIError{Code: "CLUSTERS-MGMT-CREATE-001", HTTPStatus: http.StatusBadRequest, Message: "Invalid request body"}
	ErrClusterCreateMissingFields = APIError{Code: "CLUSTERS-MGMT-CREATE-002", HTTPStatus: http.StatusBadRequest, Message: "Missing required fields: name and spec"}
	ErrClusterCreateFailed = APIError{Code: "CLUSTERS-MGMT-CREATE-003", HTTPStatus: http.StatusInternalServerError, Message: "Failed to create cluster"}
	ErrClusterCreateNameCheck = APIError{Code: "CLUSTERS-MGMT-CREATE-004", HTTPStatus: http.StatusInternalServerError, Message: "Failed to validate cluster name"}
	ErrClusterCreateOidcConfigLookupFailed = APIError{Code: "CLUSTERS-MGMT-CREATE-013", HTTPStatus: http.StatusInternalServerError, Message: "Failed to look up referenced OIDC config"}
	ErrClusterCreateNameConflict = APIError{Code: "CLUSTERS-MGMT-CREATE-005", HTTPStatus: http.StatusConflict, Message: "Cluster name already exists in this account", Reason: "a cluster named %q already exists in this account"}
	ErrClusterCreateInvalidName = APIError{Code: "CLUSTERS-MGMT-CREATE-006", HTTPStatus: http.StatusBadRequest, Message: fmt.Sprintf("Cluster metadata.name must be a valid DNS label and, without dnsReservationId, fit the HostedCluster management namespace (maximum %d characters)", seaplanev1alpha1.MaxClusterNameLengthWithoutDNSReservation())}
	ErrClusterCreateNamespaceMismatch = APIError{Code: "CLUSTERS-MGMT-CREATE-007", HTTPStatus: http.StatusBadRequest, Message: "metadata.namespace must match the authenticated account namespace"}
	ErrClusterCreateInvalidSpec = APIError{Code: "CLUSTERS-MGMT-CREATE-008", HTTPStatus: http.StatusBadRequest, Message: "Invalid cluster spec"}
	ErrClusterCreateDNSReservationRequired = APIError{Code: "CLUSTERS-MGMT-CREATE-014", HTTPStatus: http.StatusBadRequest, Message: "spec.dnsReservationId is required"}
	ErrClusterCreateDNSReservationNotFound = APIError{Code: "CLUSTERS-MGMT-CREATE-015", HTTPStatus: http.StatusNotFound, Message: "Referenced DNS reservation not found"}
	ErrClusterCreateDNSReservationNotReady = APIError{Code: "CLUSTERS-MGMT-CREATE-016", HTTPStatus: http.StatusUnprocessableEntity, Message: "Referenced DNS reservation is not ready"}
	ErrClusterCreateDNSReservationInUse = APIError{Code: "CLUSTERS-MGMT-CREATE-017", HTTPStatus: http.StatusConflict, Message: "Referenced DNS reservation is already claimed"}
	ErrClusterCreateOidcConfigRequired = APIError{Code: "CLUSTERS-MGMT-CREATE-009", HTTPStatus: http.StatusBadRequest, Message: "spec.oidcConfigId is required"}
	ErrClusterCreateOidcConfigNotFound = APIError{Code: "CLUSTERS-MGMT-CREATE-010", HTTPStatus: http.StatusNotFound, Message: "Referenced OIDC config not found"}
	ErrClusterCreateOidcConfigNotReady = APIError{Code: "CLUSTERS-MGMT-CREATE-011", HTTPStatus: http.StatusUnprocessableEntity, Message: "Referenced OIDC config is not ready"}
	ErrClusterCreateOidcConfigInUse = APIError{Code: "CLUSTERS-MGMT-CREATE-012", HTTPStatus: http.StatusConflict, Message: "Referenced OIDC config is already associated with another cluster", Reason: "an OidcConfig backs at most one cluster; %q is already in use"}

	// Cluster — Get
	ErrClusterGetNotFound = APIError{Code: "CLUSTERS-MGMT-GET-001", HTTPStatus: http.StatusNotFound, Message: "Cluster not found"}
	ErrClusterGetFailed = APIError{Code: "CLUSTERS-MGMT-GET-002", HTTPStatus: http.StatusInternalServerError, Message: "Failed to get cluster"}

	// Cluster — Update
	ErrClusterUpdateInvalidBody = APIError{Code: "CLUSTERS-MGMT-UPDATE-001", HTTPStatus: http.StatusBadRequest, Message: "Invalid request body"}
	ErrClusterUpdateMissingFields = APIError{Code: "CLUSTERS-MGMT-UPDATE-002", HTTPStatus: http.StatusBadRequest, Message: "Missing required field: spec"}
	ErrClusterUpdateNotFound = APIError{Code: "CLUSTERS-MGMT-UPDATE-003", HTTPStatus: http.StatusNotFound, Message: "Cluster not found"}
	ErrClusterUpdateFailed = APIError{Code: "CLUSTERS-MGMT-UPDATE-004", HTTPStatus: http.StatusInternalServerError, Message: "Failed to update cluster"}
	ErrClusterUpdateInvalidSpec = APIError{Code: "CLUSTERS-MGMT-UPDATE-005", HTTPStatus: http.StatusBadRequest, Message: "Invalid cluster spec"}
	ErrClusterUpdateConflict = APIError{Code: "CLUSTERS-MGMT-UPDATE-006", HTTPStatus: http.StatusConflict, Message: "Cluster was modified by a concurrent request"}
	ErrClusterUpdateNamespaceMismatch = APIError{Code: "CLUSTERS-MGMT-UPDATE-007", HTTPStatus: http.StatusBadRequest, Message: "metadata.namespace must match the authenticated account namespace"}

	// Cluster — Delete
	ErrClusterDeleteNotFound = APIError{Code: "CLUSTERS-MGMT-DELETE-001", HTTPStatus: http.StatusNotFound, Message: "Cluster not found"}
	ErrClusterDeleteFailed = APIError{Code: "CLUSTERS-MGMT-DELETE-002", HTTPStatus: http.StatusInternalServerError, Message: "Failed to delete cluster"}

	// Cluster — Status
	ErrClusterStatusNotFound = APIError{Code: "CLUSTERS-MGMT-STATUS-001", HTTPStatus: http.StatusNotFound, Message: "Cluster not found"}
	ErrClusterStatusFailed = APIError{Code: "CLUSTERS-MGMT-STATUS-002", HTTPStatus: http.StatusInternalServerError, Message: "Failed to get cluster status"}

	// Cluster — Validation
	ErrClusterValidation = APIError{Code: "CLUSTERS-MGMT-VALIDATION-001", HTTPStatus: http.StatusUnprocessableEntity, Message: "A validation error has occurred, check the errors field for more information"}

	// NodePool — List
	ErrNodePoolList = APIError{Code: "NODEPOOLS-MGMT-LIST-001", HTTPStatus: http.StatusInternalServerError, Message: "Failed to list nodepools"}
	ErrNodePoolListInvalidClusterUID = APIError{Code: "NODEPOOLS-MGMT-LIST-002", HTTPStatus: http.StatusBadRequest, Message: "clusterUID must be a valid resource UID"}
	ErrNodePoolListInvalidLabelSelector = APIError{Code: "NODEPOOLS-MGMT-LIST-003", HTTPStatus: http.StatusBadRequest, Message: "labelSelector is malformed, unsupported, or conflicts with clusterUID"}

	// NodePool — Create
	ErrNodePoolCreateInvalidBody = APIError{Code: "NODEPOOLS-MGMT-CREATE-001", HTTPStatus: http.StatusBadRequest, Message: "Invalid request body"}
	ErrNodePoolCreateMissingFields = APIError{Code: "NODEPOOLS-MGMT-CREATE-002", HTTPStatus: http.StatusBadRequest, Message: "metadata.name is required"}
	ErrNodePoolCreateNameConflict = APIError{Code: "NODEPOOLS-MGMT-CREATE-003", HTTPStatus: http.StatusConflict, Message: "NodePool already exists"}
	ErrNodePoolCreateClusterNotFound = APIError{Code: "NODEPOOLS-MGMT-CREATE-004", HTTPStatus: http.StatusNotFound, Message: "Referenced cluster not found"}
	ErrNodePoolCreateClusterCheck = APIError{Code: "NODEPOOLS-MGMT-CREATE-005", HTTPStatus: http.StatusInternalServerError, Message: "Failed to validate cluster reference"}
	ErrNodePoolCreateInvalidSpec = APIError{Code: "NODEPOOLS-MGMT-CREATE-006", HTTPStatus: http.StatusBadRequest, Message: "Invalid nodepool spec"}
	ErrNodePoolCreateFailed = APIError{Code: "NODEPOOLS-MGMT-CREATE-007", HTTPStatus: http.StatusInternalServerError, Message: "Failed to create nodepool"}
	ErrNodePoolCreateInvalidNamespace = APIError{Code: "NODEPOOLS-MGMT-CREATE-008", HTTPStatus: http.StatusBadRequest, Message: "metadata.namespace must match the authenticated account namespace"}
	ErrNodePoolCreateInvalidName = APIError{Code: "NODEPOOLS-MGMT-CREATE-009", HTTPStatus: http.StatusBadRequest, Message: "metadata.name must be <cluster>.<child> using valid DNS labels"}
	ErrNodePoolCreateClusterDeleting = APIError{Code: "NODEPOOLS-MGMT-CREATE-011", HTTPStatus: http.StatusConflict, Message: "Cannot create a NodePool for a Cluster that is deleting"}

	// NodePool — Get
	ErrNodePoolGetNotFound = APIError{Code: "NODEPOOLS-MGMT-GET-001", HTTPStatus: http.StatusNotFound, Message: "NodePool not found"}
	ErrNodePoolGetFailed = APIError{Code: "NODEPOOLS-MGMT-GET-002", HTTPStatus: http.StatusInternalServerError, Message: "Failed to get nodepool"}

	// NodePool — Update
	ErrNodePoolUpdateInvalidBody = APIError{Code: "NODEPOOLS-MGMT-UPDATE-001", HTTPStatus: http.StatusBadRequest, Message: "Invalid request body"}
	ErrNodePoolUpdateMissingFields = APIError{Code: "NODEPOOLS-MGMT-UPDATE-002", HTTPStatus: http.StatusBadRequest, Message: "Missing required field: spec"}
	ErrNodePoolUpdateNotFound = APIError{Code: "NODEPOOLS-MGMT-UPDATE-003", HTTPStatus: http.StatusNotFound, Message: "NodePool not found"}
	ErrNodePoolUpdateFailed = APIError{Code: "NODEPOOLS-MGMT-UPDATE-004", HTTPStatus: http.StatusInternalServerError, Message: "Failed to update nodepool"}
	ErrNodePoolUpdateInvalidSpec = APIError{Code: "NODEPOOLS-MGMT-UPDATE-005", HTTPStatus: http.StatusBadRequest, Message: "Invalid nodepool spec"}
	ErrNodePoolUpdateConflict = APIError{Code: "NODEPOOLS-MGMT-UPDATE-006", HTTPStatus: http.StatusConflict, Message: "NodePool was modified by a concurrent request"}
	ErrNodePoolUpdateInvalidNamespace = APIError{Code: "NODEPOOLS-MGMT-UPDATE-007", HTTPStatus: http.StatusBadRequest, Message: "metadata.namespace must match the authenticated account namespace"}

	// NodePool — Delete
	ErrNodePoolDeleteNotFound = APIError{Code: "NODEPOOLS-MGMT-DELETE-001", HTTPStatus: http.StatusNotFound, Message: "NodePool not found"}
	ErrNodePoolDeleteFailed = APIError{Code: "NODEPOOLS-MGMT-DELETE-002", HTTPStatus: http.StatusInternalServerError, Message: "Failed to delete nodepool"}

	// NodePool — Status
	ErrNodePoolStatusNotFound = APIError{Code: "NODEPOOLS-MGMT-STATUS-001", HTTPStatus: http.StatusNotFound, Message: "NodePool not found"}
	ErrNodePoolStatusFailed = APIError{Code: "NODEPOOLS-MGMT-STATUS-002", HTTPStatus: http.StatusInternalServerError, Message: "Failed to get nodepool status"}

	// NodePool — Validation
	ErrNodePoolValidation = APIError{Code: "NODEPOOLS-MGMT-VALIDATION-001", HTTPStatus: http.StatusUnprocessableEntity, Message: "A validation error has occurred, check the errors field for more information"}

	// Rendered HostedCluster — List and Get
	ErrRenderedHostedClusterList = APIError{Code: "RENDERED-CLUSTERS-MGMT-LIST-001", HTTPStatus: http.StatusInternalServerError, Message: "Failed to list rendered HostedClusters"}
	ErrRenderedHostedClusterGetNotFound = APIError{Code: "RENDERED-CLUSTERS-MGMT-GET-001", HTTPStatus: http.StatusNotFound, Message: "Rendered HostedCluster not found"}
	ErrRenderedHostedClusterGetFailed = APIError{Code: "RENDERED-CLUSTERS-MGMT-GET-002", HTTPStatus: http.StatusInternalServerError, Message: "Failed to get rendered HostedCluster"}

	// Rendered NodePool — List and Get
	ErrRenderedNodePoolList = APIError{Code: "RENDERED-NODEPOOLS-MGMT-LIST-001", HTTPStatus: http.StatusInternalServerError, Message: "Failed to list rendered NodePools"}
	ErrRenderedNodePoolGetNotFound = APIError{Code: "RENDERED-NODEPOOLS-MGMT-GET-001", HTTPStatus: http.StatusNotFound, Message: "Rendered NodePool not found"}
	ErrRenderedNodePoolGetFailed = APIError{Code: "RENDERED-NODEPOOLS-MGMT-GET-002", HTTPStatus: http.StatusInternalServerError, Message: "Failed to get rendered NodePool"}

	// OidcConfig — List
	ErrOidcConfigList = APIError{Code: "OIDCCONFIGS-MGMT-LIST-001", HTTPStatus: http.StatusInternalServerError, Message: "Failed to list OIDC configs"}

	// OidcConfig — Create
	ErrOidcConfigCreateInvalidBody = APIError{Code: "OIDCCONFIGS-MGMT-CREATE-001", HTTPStatus: http.StatusBadRequest, Message: "Invalid request body"}
	ErrOidcConfigCreateMissingFields = APIError{Code: "OIDCCONFIGS-MGMT-CREATE-002", HTTPStatus: http.StatusBadRequest, Message: "metadata.name and spec.type are required"}
	ErrOidcConfigCreateFailed = APIError{Code: "OIDCCONFIGS-MGMT-CREATE-003", HTTPStatus: http.StatusInternalServerError, Message: "Failed to create OIDC config"}
	ErrOidcConfigCreateInvalidName = APIError{Code: "OIDCCONFIGS-MGMT-CREATE-009", HTTPStatus: http.StatusBadRequest, Message: "metadata.name must be a valid DNS subdomain"}
	ErrOidcConfigCreateNameConflict = APIError{Code: "OIDCCONFIGS-MGMT-CREATE-010", HTTPStatus: http.StatusConflict, Message: "An OIDC config with this name already exists"}
	ErrOidcConfigCreateNamespaceMismatch = APIError{Code: "OIDCCONFIGS-MGMT-CREATE-011", HTTPStatus: http.StatusBadRequest, Message: "metadata.namespace must match the authenticated account namespace"}
	ErrOidcConfigCreateInvalidType = APIError{Code: "OIDCCONFIGS-MGMT-CREATE-004", HTTPStatus: http.StatusBadRequest, Message: "spec.type must be 'managed' or 'unmanaged'"}
	ErrOidcConfigCreateInvalidFields = APIError{Code: "OIDCCONFIGS-MGMT-CREATE-005", HTTPStatus: http.StatusBadRequest, Message: "unmanaged type requires secretArn and installerRoleArn; managed type must not set them"}
	ErrOidcConfigCreateDuplicateIssuerUrl = APIError{Code: "OIDCCONFIGS-MGMT-CREATE-006", HTTPStatus: http.StatusConflict, Message: "An OIDC config with this issuerUrl already exists"}
	ErrOidcConfigCreateIssuerNotConfigured = APIError{Code: "OIDCCONFIGS-MGMT-CREATE-007", HTTPStatus: http.StatusInternalServerError, Message: "Server is not configured with an OIDC issuer base URL"}
	ErrOidcConfigCreateInvalidIssuerUrl = APIError{Code: "OIDCCONFIGS-MGMT-CREATE-008", HTTPStatus: http.StatusBadRequest, Message: "spec.issuerUrl is invalid", Reason: "%s"}

	// OidcConfig — Get
	ErrOidcConfigGetNotFound = APIError{Code: "OIDCCONFIGS-MGMT-GET-001", HTTPStatus: http.StatusNotFound, Message: "OIDC config not found"}
	ErrOidcConfigGetFailed = APIError{Code: "OIDCCONFIGS-MGMT-GET-002", HTTPStatus: http.StatusInternalServerError, Message: "Failed to get OIDC config"}

	// OidcConfig — Delete
	ErrOidcConfigDeleteNotFound = APIError{Code: "OIDCCONFIGS-MGMT-DELETE-001", HTTPStatus: http.StatusNotFound, Message: "OIDC config not found"}
	ErrOidcConfigDeleteFailed = APIError{Code: "OIDCCONFIGS-MGMT-DELETE-002", HTTPStatus: http.StatusInternalServerError, Message: "Failed to delete OIDC config"}
	ErrOidcConfigDeleteInUse = APIError{Code: "OIDCCONFIGS-MGMT-DELETE-003", HTTPStatus: http.StatusConflict, Message: "Cannot delete OIDC config referenced by clusters"}

	// DNSReservation — List
	ErrDNSReservationListFailed = APIError{Code: "DNSRESERVATIONS-MGMT-LIST-001", HTTPStatus: http.StatusInternalServerError, Message: "Failed to list DNS reservations"}

	// DNSReservation — Create
	ErrDNSReservationCreateInvalidBody = APIError{Code: "DNSRESERVATIONS-MGMT-CREATE-001", HTTPStatus: http.StatusBadRequest, Message: "Invalid request body"}
	ErrDNSReservationCreateMissingName = APIError{Code: "DNSRESERVATIONS-MGMT-CREATE-002", HTTPStatus: http.StatusBadRequest, Message: "metadata.name is required"}
	ErrDNSReservationCreateInvalidName = APIError{Code: "DNSRESERVATIONS-MGMT-CREATE-003", HTTPStatus: http.StatusBadRequest, Message: "metadata.name must be a valid DNS subdomain"}
	ErrDNSReservationCreateNamespaceMismatch = APIError{Code: "DNSRESERVATIONS-MGMT-CREATE-004", HTTPStatus: http.StatusBadRequest, Message: "metadata.namespace must match the authenticated account namespace"}
	ErrDNSReservationCreateNameConflict = APIError{Code: "DNSRESERVATIONS-MGMT-CREATE-005", HTTPStatus: http.StatusConflict, Message: "A DNS reservation with this name already exists"}
	ErrDNSReservationCreateFailed = APIError{Code: "DNSRESERVATIONS-MGMT-CREATE-006", HTTPStatus: http.StatusInternalServerError, Message: "Failed to create DNS reservation"}

	// DNSReservation — Get
	ErrDNSReservationGetNotFound = APIError{Code: "DNSRESERVATIONS-MGMT-GET-001", HTTPStatus: http.StatusNotFound, Message: "DNS reservation not found"}
	ErrDNSReservationGetFailed = APIError{Code: "DNSRESERVATIONS-MGMT-GET-002", HTTPStatus: http.StatusInternalServerError, Message: "Failed to get DNS reservation"}

	// DNSReservation — Delete
	ErrDNSReservationDeleteNotFound = APIError{Code: "DNSRESERVATIONS-MGMT-DELETE-001", HTTPStatus: http.StatusNotFound, Message: "DNS reservation not found"}
	ErrDNSReservationDeleteClaimed = APIError{Code: "DNSRESERVATIONS-MGMT-DELETE-002", HTTPStatus: http.StatusConflict, Message: "Cannot delete a claimed DNS reservation"}
	ErrDNSReservationDeleteFailed = APIError{Code: "DNSRESERVATIONS-MGMT-DELETE-003", HTTPStatus: http.StatusInternalServerError, Message: "Failed to delete DNS reservation"}

	// Account allowlist
	ErrAccountsForbidden = APIError{Code: "ACCOUNTS-MGMT-001", HTTPStatus: http.StatusForbidden, Message: "Only the platform account may manage the account allowlist"}
	ErrAccountsInvalidID = APIError{Code: "ACCOUNTS-MGMT-002", HTTPStatus: http.StatusBadRequest, Message: "accountId must be a 12-digit AWS account ID"}
	ErrAccountsFailed = APIError{Code: "ACCOUNTS-MGMT-003", HTTPStatus: http.StatusInternalServerError, Message: "Failed to update the account allowlist"}

	// Info
	ErrInfoRegionalAccountUnavailable = APIError{Code: "INFO-001", HTTPStatus: http.StatusServiceUnavailable, Message: "regional account ID is not configured"}

	// Router fallbacks
	ErrRouteNotFound = APIError{Code: "ROUTE-001", HTTPStatus: http.StatusNotFound, Message: "route not found"}
	ErrRouteMethodNotAllowed = APIError{Code: "ROUTE-002", HTTPStatus: http.StatusMethodNotAllowed, Message: "method not allowed"}
}
