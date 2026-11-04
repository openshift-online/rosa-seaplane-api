package authz

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ResolvedAttachment pairs an attachment with the policy it references.
type ResolvedAttachment struct {
	Attachment StoredAttachment
	Policy     StoredPolicy
}

// Resolve reads a caller's global and local regional attachments and their policies.
// It returns an error instead of partial results if any read or validation fails.
func (s *Store) Resolve(ctx context.Context, id Identity) ([]ResolvedAttachment, error) {
	return s.resolveScopes(ctx, id, []AttachmentScope{GlobalScope, RegionalScope})
}

// resolveScopes loads only the grant scopes permitted to authorize an operation.
func (s *Store) resolveScopes(ctx context.Context, id Identity, scopes []AttachmentScope) ([]ResolvedAttachment, error) {
	caller, err := checkedIdentity(id)
	if err != nil {
		return nil, err
	}
	if caller.account != s.config.AccountID || id.IsAllowlisted {
		return nil, fmt.Errorf("store identity must match account without allowlist bypass")
	}
	keys := []string{principalKey(caller)}
	if caller.kind == "assumed-role" {
		keys = append(keys, roleKey(caller))
	}
	bindings := []StoredAttachment{}
	aliases := map[string]string{}
	bindingIDs := map[string]bool{}
	for _, scope := range scopes {
		for _, pk := range keys {
			part, err := s.readBindings(ctx, scope, pk)
			if err != nil {
				return nil, err
			}
			for _, binding := range part {
				principal, err := parsePrincipal(binding.PrincipalARN)
				if err != nil {
					return nil, err
				}
				if !matchesPrincipal(principal, caller) {
					return nil, fmt.Errorf("principal lookup mismatch")
				}
				if principal.kind == "role" {
					if err := addRoleAlias(aliases, principal.roleAlias(), principal.original); err != nil {
						return nil, err
					}
				}
				diagnosticID := bindingDiagnosticID(binding)
				if bindingIDs[diagnosticID] {
					return nil, fmt.Errorf("duplicate attachment identity")
				}
				bindingIDs[diagnosticID] = true
				bindings = append(bindings, binding)
			}
		}
	}
	policies, err := s.loadPolicies(ctx, bindings)
	if err != nil {
		return nil, err
	}
	resolved := make([]ResolvedAttachment, 0, len(bindings))
	for _, binding := range bindings {
		policy, exists := policies[binding.Policy]
		if !exists {
			return nil, fmt.Errorf("referenced policy %s: %w", binding.Policy.ID, ErrNotFound)
		}
		resolved = append(resolved, ResolvedAttachment{Attachment: binding, Policy: policy})
	}
	return resolved, nil
}

// loadPolicies returns the policies referenced by the attachments, loading each distinct policy once.
// It returns an error if a read fails or unprocessed keys remain after retries.
func (s *Store) loadPolicies(ctx context.Context, bindings []StoredAttachment) (map[PolicyRef]StoredPolicy, error) {
	const batchSize = 100
	const batchAttempts = 4
	const retryDelay = 100 * time.Millisecond
	refs := []PolicyRef{}
	seen := map[PolicyRef]bool{}
	for _, binding := range bindings {
		if !seen[binding.Policy] {
			refs = append(refs, binding.Policy)
			seen[binding.Policy] = true
		}
	}
	policies := map[PolicyRef]StoredPolicy{}
	for offset := 0; offset < len(refs); offset += batchSize {
		keys := []map[string]types.AttributeValue{}
		for _, ref := range refs[offset:min(offset+batchSize, len(refs))] {
			owner, err := s.policyOwner(ref)
			if err != nil {
				return nil, err
			}
			keys = append(keys, storeKey("ACCOUNT#"+owner, "POLICY#"+ref.ID))
		}
		pending := map[string]types.KeysAndAttributes{s.config.PolicyTable: {Keys: keys, ConsistentRead: aws.Bool(true)}}
		for attempt := 0; len(pending) > 0 && attempt < batchAttempts; attempt++ {
			// Back off before retrying unprocessed keys without delaying cancellation.
			if attempt > 0 {
				timer := time.NewTimer(retryDelay * time.Duration(1<<(attempt-1)))
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				case <-timer.C:
				}
			}
			out, err := s.client.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: pending})
			if err != nil {
				return nil, backendError(err)
			}
			for _, item := range out.Responses[s.config.PolicyTable] {
				policy, err := s.decodePolicy(item)
				if err != nil {
					return nil, err
				}
				ref := PolicyRef{ID: policy.ID, Kind: policy.Kind}
				if !seen[ref] {
					return nil, fmt.Errorf("inconsistent referenced policy")
				}
				if _, exists := policies[ref]; exists {
					return nil, fmt.Errorf("duplicate referenced policy")
				}
				policies[ref] = policy
			}
			pending = out.UnprocessedKeys
		}
		if len(pending) > 0 {
			return nil, fmt.Errorf("%w: incomplete policy batch after %d attempts", ErrUnavailable, batchAttempts)
		}
	}
	return policies, nil
}

// bindingDiagnosticID returns an attachment's scope, region, and ID joined for diagnostics.
func bindingDiagnosticID(binding StoredAttachment) string {
	return string(binding.Scope) + "/" + binding.Region + "/" + binding.ID
}

// Prepare loads and binds stored policies for one caller and request.
func (s *Store) Prepare(ctx context.Context, id Identity, req RequestContext) (*Prepared, error) {
	return s.prepareScopes(ctx, id, req, []AttachmentScope{GlobalScope, RegionalScope})
}

// prepareScopes shares binding and Cedar preparation without a management allowlist bypass.
func (s *Store) prepareScopes(ctx context.Context, id Identity, req RequestContext, scopes []AttachmentScope) (*Prepared, error) {
	resolved, err := s.resolveScopes(ctx, id, scopes)
	if err != nil {
		return nil, managementResultResolution(err)
	}
	authorizer := &Authorizer{region: s.config.Region, model: s.model, validator: s.validator, evaluate: nativeEvaluate}
	for _, record := range resolved {
		binding, policy := record.Attachment, record.Policy
		principal, err := parsePrincipal(binding.PrincipalARN)
		if err != nil {
			return nil, newFailure(StageBinding, err)
		}
		bound, err := s.bindPolicy(policy.Content, policy.ID, principal)
		if err != nil {
			return nil, newFailure(StageBinding, err)
		}
		diagnosticID := bindingDiagnosticID(binding)
		authorizer.customer = append(authorizer.customer, policyAttachment{Provenance: Provenance{DiagnosticID: diagnosticID, PolicyID: policy.ID, AttachmentID: binding.ID, PolicyRevision: policy.Revision, AttachmentRevision: binding.Revision}, policy: bound, principal: principal, scope: string(binding.Scope), region: binding.Region})
	}
	return authorizer.Prepare(ctx, id, req)
}
