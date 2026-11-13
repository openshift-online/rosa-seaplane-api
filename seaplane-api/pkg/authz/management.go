package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

var (
	// ErrManagementDenied indicates that the caller lacks a management grant.
	ErrManagementDenied = errors.New("management permission denied")
	// ErrValidation indicates invalid management input.
	ErrValidation = errors.New("invalid authorization input")
	// ErrManagedPolicy indicates an attempt to mutate a platform policy.
	ErrManagedPolicy = errors.New("managed policies are immutable")
	// ErrUnavailable indicates a backend I/O failure.
	ErrUnavailable = errors.New("authorization store unavailable")
)

// ManagementCaller carries the authenticated identity and request context.
type ManagementCaller struct {
	AccountID, CallerARN string
	Request              RequestContext
}

// DeletionReceipt identifies the committed deletion and its local region.
type DeletionReceipt struct {
	ID              string `json:"id"`
	DeletedRevision string `json:"deletedRevision"`
	CommittedRegion string `json:"committedRegion"`
}

// Service manages account-scoped policies and prepares resource authorization.
// Enrollment is enforced by the HTTP layer before any Service call.
type Service struct {
	base Store // Shared client, tables, region, and schema; the account is set per request.
}

// NewService validates configuration and loads the Cedar schema once for all accounts.
func NewService(client *dynamodb.Client, cfg ServiceConfig, region string) (*Service, error) {
	if err := checkServiceConfig(cfg); err != nil {
		return nil, err
	}
	if client == nil || !regionPattern.MatchString(region) {
		return nil, fmt.Errorf("%w: client and service region required", ErrValidation)
	}
	model, validator, err := loadSchema()
	if err != nil {
		return nil, err
	}
	base := Store{client: client, model: model, validator: validator, config: StoreConfig{
		Region: region, PolicyTable: cfg.PolicyTable, GlobalTable: cfg.GlobalTable, RegionalTable: cfg.RegionalTable,
	}}
	return &Service{base: base}, nil
}

// store returns the account's view of the shared tables, whose rows are keyed by account.
func (m *Service) store(account string) *Store {
	if !accountPattern.MatchString(account) {
		return nil
	}
	store := m.base
	store.config.AccountID = account
	return &store
}

// authorize checks the caller identity and scope-specific Cedar grants.
// The first grants for an account are seeded directly into storage (see docs/authz/storage-contract.md).
func (m *Service) authorize(ctx context.Context, caller ManagementCaller, action Action, id string) (*Store, error) {
	store := m.store(caller.AccountID)
	identity := Identity{AccountID: caller.AccountID, CallerARN: caller.CallerARN}
	if _, err := checkedIdentity(identity); err != nil || store == nil {
		return nil, ErrManagementDenied
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	scopes := []AttachmentScope{GlobalScope}
	if isRegionalAction(action) {
		scopes = append(scopes, RegionalScope)
	}
	prepared, err := store.prepareScopes(ctx, identity, caller.Request, scopes)
	if err != nil {
		return nil, err
	}
	kind := actionResourceKind(action)
	resource := Resource{Kind: kind, AccountID: caller.AccountID, ID: id}
	if listAction(action) {
		resource.Kind, resource.CollectionKind, resource.ID = Collection, kind, ""
	}
	decision, err := prepared.Check(ctx, action, resource)
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		return nil, ErrManagementDenied
	}
	return store, nil
}

// managementResult preserves classified errors and fails closed on malformed stored state.
func managementResult(err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{ErrValidation, ErrNotFound, ErrConflict, ErrUnavailable, ErrManagedPolicy} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	var failure *Failure
	if errors.As(err, &failure) {
		return err
	}
	return newFailure(StageResolution, err)
}

// managementResultResolution distinguishes backend outages from failed grant resolution.
func managementResultResolution(err error) error {
	if errors.Is(err, ErrUnavailable) {
		return err
	}
	return newFailure(StageResolution, err)
}

// backendError marks SDK I/O failures without inspecting error messages.
func backendError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// checkPolicyRef validates an external policy reference.
func checkPolicyRef(ref PolicyRef) error {
	if !isStoreID(ref.ID) || (ref.Kind != CustomPolicy && ref.Kind != ManagedPolicy) {
		return fmt.Errorf("%w: invalid policy reference", ErrValidation)
	}
	return nil
}

// checkPolicyMutation rejects managed writes and invalid expected revisions.
func checkPolicyMutation(ref PolicyRef, expected string) error {
	if err := checkPolicyRef(ref); err != nil {
		return err
	}
	if ref.Kind == ManagedPolicy {
		return ErrManagedPolicy
	}
	if !isRevision(expected) {
		return fmt.Errorf("%w: expected revision required", ErrValidation)
	}
	return nil
}

// CreatePolicy validates and commits a custom policy in the service region.
func (m *Service) CreatePolicy(ctx context.Context, caller ManagementCaller, id, content string) (StoredPolicy, error) {
	store, err := m.authorize(ctx, caller, CreatePolicy, id)
	if err != nil {
		return StoredPolicy{}, err
	}
	policy, err := store.CreatePolicy(ctx, id, content)
	return policy, managementResult(err)
}

// ListPolicies returns managed and account-owned custom policies without merging colliding IDs.
func (m *Service) ListPolicies(ctx context.Context, caller ManagementCaller) ([]StoredPolicy, error) {
	store, err := m.authorize(ctx, caller, ListPolicies, "")
	if err != nil {
		return nil, err
	}
	policies, err := store.ListPolicies(ctx)
	return policies, managementResult(err)
}

// GetPolicy returns the explicitly selected custom or managed policy.
func (m *Service) GetPolicy(ctx context.Context, caller ManagementCaller, ref PolicyRef) (StoredPolicy, error) {
	if err := checkPolicyRef(ref); err != nil {
		return StoredPolicy{}, err
	}
	store, err := m.authorize(ctx, caller, DescribePolicy, ref.ID)
	if err != nil {
		return StoredPolicy{}, err
	}
	policy, err := store.GetPolicy(ctx, ref)
	return policy, managementResult(err)
}

// UpdatePolicy replaces a custom policy only at the expected revision.
func (m *Service) UpdatePolicy(ctx context.Context, caller ManagementCaller, ref PolicyRef, expected, content string) (StoredPolicy, error) {
	if err := checkPolicyMutation(ref, expected); err != nil {
		return StoredPolicy{}, err
	}
	store, err := m.authorize(ctx, caller, UpdatePolicy, ref.ID)
	if err != nil {
		return StoredPolicy{}, err
	}
	policy, err := store.UpdatePolicy(ctx, ref.ID, expected, content)
	return policy, managementResult(err)
}

// DeletePolicy deletes a custom policy only at the expected revision.
func (m *Service) DeletePolicy(ctx context.Context, caller ManagementCaller, ref PolicyRef, expected string) (DeletionReceipt, error) {
	if err := checkPolicyMutation(ref, expected); err != nil {
		return DeletionReceipt{}, err
	}
	store, err := m.authorize(ctx, caller, DeletePolicy, ref.ID)
	if err != nil {
		return DeletionReceipt{}, err
	}
	if err := store.DeletePolicy(ctx, ref.ID, expected); err != nil {
		return DeletionReceipt{}, managementResult(err)
	}
	return DeletionReceipt{ID: ref.ID, DeletedRevision: expected, CommittedRegion: store.config.Region}, nil
}

// attachmentAction selects the separate global or regional permission.
func attachmentAction(scope AttachmentScope, global, regional Action) (Action, error) {
	switch scope {
	case GlobalScope:
		return global, nil
	case RegionalScope:
		return regional, nil
	default:
		return "", fmt.Errorf("%w: invalid attachment scope", ErrValidation)
	}
}

// CreateAttachment validates ownership and commits a new scoped attachment.
func (m *Service) CreateAttachment(ctx context.Context, caller ManagementCaller, spec AttachmentSpec) (StoredAttachment, error) {
	action, err := attachmentAction(spec.Scope, CreateAttachment, CreateAttachmentRegional)
	if err != nil {
		return StoredAttachment{}, err
	}
	store, err := m.authorize(ctx, caller, action, "")
	if err != nil {
		return StoredAttachment{}, err
	}
	if err := checkPolicyRef(spec.Policy); err != nil {
		return StoredAttachment{}, err
	}
	principal, err := parsePrincipal(spec.PrincipalARN)
	if err != nil || principal.account != caller.AccountID {
		return StoredAttachment{}, fmt.Errorf("%w: principal must belong to the caller account", ErrValidation)
	}
	binding, err := store.CreateAttachment(ctx, spec)
	return binding, managementResult(err)
}

// ListAttachments requires both permissions for an unfiltered list and never silently omits a scope.
func (m *Service) ListAttachments(ctx context.Context, caller ManagementCaller, scope AttachmentScope) ([]StoredAttachment, error) {
	scopes := []AttachmentScope{scope}
	if scope == "" {
		scopes = []AttachmentScope{GlobalScope, RegionalScope}
	}
	var store *Store
	for _, scope := range scopes {
		action, err := attachmentAction(scope, ListAttachments, ListAttachmentsRegional)
		if err != nil {
			return nil, err
		}
		store, err = m.authorize(ctx, caller, action, "")
		if err != nil {
			return nil, err
		}
	}
	bindings := []StoredAttachment{}
	for _, scope := range scopes {
		part, err := store.readBindings(ctx, scope, "ACCOUNT#"+caller.AccountID)
		if err != nil {
			return nil, managementResult(err)
		}
		bindings = append(bindings, part...)
	}
	return bindings, nil
}

// DeleteAttachment removes a scoped attachment only at the expected revision.
func (m *Service) DeleteAttachment(ctx context.Context, caller ManagementCaller, scope AttachmentScope, id, expected string) (DeletionReceipt, error) {
	action, err := attachmentAction(scope, DeleteAttachment, DeleteAttachmentRegional)
	if err != nil {
		return DeletionReceipt{}, err
	}
	if !isStoreID(id) || !isRevision(expected) {
		return DeletionReceipt{}, fmt.Errorf("%w: valid attachment ID and revision required", ErrValidation)
	}
	store, err := m.authorize(ctx, caller, action, id)
	if err != nil {
		return DeletionReceipt{}, err
	}
	if err := store.DeleteAttachment(ctx, scope, id, expected); err != nil {
		return DeletionReceipt{}, managementResult(err)
	}
	return DeletionReceipt{ID: id, DeletedRevision: expected, CommittedRegion: store.config.Region}, nil
}
