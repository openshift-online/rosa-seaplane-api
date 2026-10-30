package authz

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	cedar "github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/x/exp/schema/resolved"
	"github.com/cedar-policy/cedar-go/x/exp/schema/validate"
	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
)

type evaluator func(*cedar.PolicySet, cedar.EntityMap, cedar.Request) (cedar.Decision, cedar.Diagnostic, error)

// Authorizer selects validated policies once per request in a fixed service region.
type Authorizer struct {
	bundleHash string
	customer   []policyAttachment
	allowlist  *cedar.Policy
	region     string
	accounts   map[string]struct{}
	model      *resolved.Schema
	validator  *validate.Validator
	evaluate   evaluator
}

// Prepared owns concrete immutable policy inputs for one identity and request.
type Prepared struct {
	identity       Identity
	region         string
	principal      cedar.EntityUID
	entities       cedar.EntityMap
	policies       *cedar.PolicySet
	requestContext cedar.Record
	provenance     map[cedar.PolicyID]Provenance
	validator      *validate.Validator
	evaluate       evaluator
}

// BundleHash returns the SHA-256 hash of the bundle file loaded at startup.
func (a *Authorizer) BundleHash() string { return a.bundleHash }

// IsAccountRegistered checks enrollment in the startup snapshot, not permission grants.
// A canceled context returns false.
func (a *Authorizer) IsAccountRegistered(ctx context.Context, account string) bool {
	if ctx.Err() != nil {
		return false
	}
	_, exists := a.accounts[account]
	return exists
}

// nativeEvaluate returns Cedar's decision and diagnostics for the supplied policies, entities, and request.
func nativeEvaluate(set *cedar.PolicySet, entities cedar.EntityMap, req cedar.Request) (cedar.Decision, cedar.Diagnostic, error) {
	decision, diagnostics := cedar.Authorize(set, entities, req)
	return decision, diagnostics, nil
}

// Prepare freezes applicable policies, caller identity, and request context for all checks in one request.
func (a *Authorizer) Prepare(ctx context.Context, id Identity, req RequestContext) (*Prepared, error) {
	if err := ctx.Err(); err != nil {
		return nil, newFailure(StageResolution, err)
	}
	caller, err := checkedIdentity(id)
	if err != nil {
		return nil, newFailure(StageResolution, err)
	}
	if req.RequestTime.IsZero() {
		return nil, newFailure(StageResolution, fmt.Errorf("request time is required"))
	}
	timestamp := req.RequestTime.UTC()
	day := int(timestamp.Weekday())
	if day == 0 {
		day = 7
	}
	requestLabels := entityUID("RequestLabels", "request")
	prepared := &Prepared{identity: id, region: a.region, principal: entityUID("Principal", id.CallerARN), entities: make(cedar.EntityMap), policies: cedar.NewPolicySet(), provenance: make(map[cedar.PolicyID]Provenance), validator: a.validator, evaluate: a.evaluate,
		requestContext: cedar.NewRecord(cedar.RecordMap{
			"region": cedar.String(a.region), "accountId": cedar.String(id.AccountID), "principalArn": cedar.String(id.CallerARN),
			"sourceIp": cedar.String(req.SourceIP), "userAgent": cedar.String(req.UserAgent),
			"requestTime":   cedar.NewRecord(cedar.RecordMap{"unixSeconds": cedar.Long(timestamp.Unix()), "dayOfWeek": cedar.Long(day), "hour": cedar.Long(timestamp.Hour())}),
			"requestLabels": requestLabels,
		}),
	}
	prepared.entities[requestLabels] = cedar.Entity{UID: requestLabels, Tags: resourceTags(req.RequestLabels)}
	for uid, action := range a.model.Actions {
		prepared.entities[uid] = action.Entity
	}
	attrs := cedar.NewRecord(cedar.RecordMap{"account": cedar.String(id.AccountID)})
	parents := []cedar.EntityUID{}
	for _, binding := range a.customer {
		if !matchesPrincipal(binding.principal, caller) || !appliesInRegion(binding.scope, binding.region, a.region) {
			continue
		}
		diagnosticID := cedar.PolicyID(binding.DiagnosticID)
		prepared.policies.Add(diagnosticID, binding.policy)
		prepared.provenance[diagnosticID] = binding.Provenance
		if binding.principal.kind == "role" {
			role := entityUID("Role", binding.principal.original)
			prepared.entities[role] = cedar.Entity{UID: role, Attributes: attrs}
			parents = append(parents, role)
		}
	}
	// Checks pin resources to the caller's account, so this grant covers only its own resources.
	if id.IsAllowlisted {
		diagnosticID := cedar.PolicyID("allowlist/" + id.AccountID)
		prepared.policies.Add(diagnosticID, a.allowlist)
		prepared.provenance[diagnosticID] = Provenance{DiagnosticID: string(diagnosticID), PolicyID: "allowlist", BundleHash: a.bundleHash}
	}
	prepared.entities[prepared.principal] = cedar.Entity{UID: prepared.principal, Parents: cedar.NewEntityUIDSet(parents...), Attributes: attrs}
	if err := prepared.validator.Entities(prepared.entities); err != nil {
		return nil, newFailure(StageEntityValidation, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, newFailure(StageResolution, err)
	}
	return prepared, nil
}

// Check evaluates an action and trusted resource for the prepared caller, rejecting Cedar evaluation errors.
func (p *Prepared) Check(ctx context.Context, action Action, resource Resource) (decision Decision, err error) {
	// Record which check failed; one request can check several actions.
	defer func() {
		var failure *Failure
		if errors.As(err, &failure) {
			failure.Action = action
		}
	}()
	if err := ctx.Err(); err != nil {
		return Decision{}, newFailure(StageEvaluation, err)
	}
	entities, resourceUID, err := p.resourceEntities(action, resource)
	if err != nil {
		return Decision{}, newFailure(StageEntityValidation, err)
	}
	req := cedar.Request{Principal: p.principal, Action: entityUID("Action", string(action)), Resource: resourceUID, Context: p.requestContext}
	if err := p.validator.Entities(entities); err != nil {
		return Decision{}, newFailure(StageEntityValidation, err)
	}
	if err := p.validator.Request(req); err != nil {
		return Decision{}, newFailure(StageEntityValidation, err)
	}
	verdict, diagnostics, err := p.evaluate(p.policies, entities, req)
	if err != nil {
		return Decision{}, newFailure(StageEvaluation, err)
	}
	// Native Cedar may allow despite a failing forbid. Errors always take precedence.
	if len(diagnostics.Errors) > 0 {
		failure := newFailure(StageEvaluation, errors.New("cedar evaluation diagnostics"))
		failure.Diagnostics = append([]cedar.DiagnosticError(nil), diagnostics.Errors...)
		ids := make([]cedar.PolicyID, 0, len(diagnostics.Errors))
		for _, diagnostic := range diagnostics.Errors {
			ids = append(ids, diagnostic.PolicyID)
		}
		var mappingErr error
		failure.Provenance, mappingErr = p.mapProvenance(ids)
		failure.Err = errors.Join(failure.Err, mappingErr)
		return Decision{}, failure
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, newFailure(StageEvaluation, err)
	}
	ids := make([]cedar.PolicyID, 0, len(diagnostics.Reasons))
	for _, reason := range diagnostics.Reasons {
		ids = append(ids, reason.PolicyID)
	}
	provenance, err := p.mapProvenance(ids)
	if err != nil {
		return Decision{}, newFailure(StageEvaluation, err)
	}
	return Decision{Allowed: verdict == cedar.Allow, Provenance: provenance}, nil
}

// mapProvenance maps sorted, unique Cedar policy IDs to attachment provenance and reports unknown IDs.
func (p *Prepared) mapProvenance(ids []cedar.PolicyID) ([]Provenance, error) {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	provenance := make([]Provenance, 0, len(ids))
	var mappingErr error
	for _, id := range ids {
		binding, exists := p.provenance[id]
		if !exists {
			mappingErr = errors.Join(mappingErr, fmt.Errorf("unknown diagnostic policy ID %q", id))
			continue
		}
		provenance = append(provenance, binding)
	}
	return provenance, mappingErr
}

// validateResourceIdentity checks action compatibility, ownership, and collection or object identity before building entities.
func (p *Prepared) validateResourceIdentity(action Action, resource Resource) (ResourceKind, error) {
	kind := actionResourceKind(action)
	// Label actions share the four resource kinds.
	if action == ListLabelsForResource || action == LabelResource || action == UnlabelResource {
		switch resource.Kind {
		case Cluster, NodePool, OIDCConfig, DNSReservation:
			kind = resource.Kind
		}
	}
	if kind == "" {
		return "", fmt.Errorf("unsupported action")
	}
	if resource.AccountID != p.identity.AccountID {
		return "", fmt.Errorf("resource ownership does not match identity account")
	}
	if owner, exists := resource.Labels[seaplanev1alpha1.AccountIDLabel]; exists && owner != resource.AccountID {
		return "", fmt.Errorf("resource account label contradicts ownership")
	}
	if resource.Kind != NodePool && resource.ParentCluster != nil {
		return "", fmt.Errorf("unexpected parent cluster")
	}
	if listAction(action) {
		if resource.Kind != Collection || resource.ID != "" || len(resource.Labels) != 0 || resource.ParentCluster != nil || resource.CollectionKind != kind {
			return "", fmt.Errorf("invalid collection identity or object metadata")
		}
	} else if resource.Kind != kind || !idPattern.MatchString(resource.ID) || resource.CollectionKind != "" {
		return "", fmt.Errorf("action requires a compatible resource with a stable ID")
	}
	return kind, nil
}

// resourceEntities builds request-local Cedar entities and the resource UID from trusted facts and the fixed service region.
func (p *Prepared) resourceEntities(action Action, resource Resource) (cedar.EntityMap, cedar.EntityUID, error) {
	kind, err := p.validateResourceIdentity(action, resource)
	if err != nil {
		return nil, cedar.EntityUID{}, err
	}
	entities := maps.Clone(p.entities)
	attrs := cedar.NewRecord(cedar.RecordMap{"account": cedar.String(resource.AccountID), "region": cedar.String(p.region)})
	collection := entityUID("Collection", resource.AccountID+"/"+p.region+"/"+collectionName(kind))
	entities[collection] = cedar.Entity{UID: collection, Attributes: attrs}
	if listAction(action) {
		return entities, collection, nil
	}
	id := resource.AccountID + "/" + p.region + "/" + resource.ID
	parents := []cedar.EntityUID{collection}
	if kind == NodePool {
		parent := resource.ParentCluster
		if parent == nil || !idPattern.MatchString(parent.ID) || parent.AccountID != resource.AccountID {
			return nil, cedar.EntityUID{}, fmt.Errorf("node pool requires its owned stored parent")
		}
		if owner, exists := parent.Labels[seaplanev1alpha1.AccountIDLabel]; exists && owner != parent.AccountID {
			return nil, cedar.EntityUID{}, fmt.Errorf("parent account label contradicts ownership")
		}
		clusterCollection := entityUID("Collection", parent.AccountID+"/"+p.region+"/clusters")
		entities[clusterCollection] = cedar.Entity{UID: clusterCollection, Attributes: attrs}
		cluster := entityUID("Cluster", parent.AccountID+"/"+p.region+"/"+parent.ID)
		entities[cluster] = cedar.Entity{UID: cluster, Attributes: attrs, Parents: cedar.NewEntityUIDSet(clusterCollection), Tags: resourceTags(parent.Labels)}
		parents = append(parents, cluster)
		id = resource.AccountID + "/" + p.region + "/" + parent.ID + "/" + resource.ID
	}
	uid := entityUID(string(kind), id)
	entities[uid] = cedar.Entity{UID: uid, Attributes: attrs, Parents: cedar.NewEntityUIDSet(parents...), Tags: resourceTags(resource.Labels)}
	return entities, uid, nil
}

// resourceTags converts resource labels into Cedar entity tags.
func resourceTags(labels map[string]string) cedar.Record {
	tags := make(cedar.RecordMap, len(labels))
	for key, value := range labels {
		tags[cedar.String(key)] = cedar.String(value)
	}
	return cedar.NewRecord(tags)
}
