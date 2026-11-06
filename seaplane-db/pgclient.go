package seaplanedb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/model"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/reader"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/writer"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type pgClient struct {
	scheme     *runtime.Scheme
	pool       *pgxpool.Pool
	restMapper meta.RESTMapper
}

func (c *pgClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	gvk, err := resolveGVK(c.scheme, obj)
	if err != nil {
		return err
	}
	gvkStr := gvkToString(gvk)

	poolConn, err := c.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer poolConn.Release()
	conn := poolConn.Conn()

	var r model.Resource
	err = conn.QueryRow(ctx, `
		SELECT gvk, namespace, name, uid, txid_stamp::text::bigint,
		       object_version, spec, status, metadata,
		       deletion_timestamp, created_at, updated_at
		FROM kubernetes_resources
		WHERE gvk = $1 AND namespace = $2 AND name = $3`,
		gvkStr, key.Namespace, key.Name,
	).Scan(
		&r.GVK, &r.Namespace, &r.Name, &r.UID,
		&r.TxidStamp, &r.ObjectVersion, &r.Spec, &r.Status,
		&r.Metadata, &r.DeletionTimestamp, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return mapGetError(err, gvk, key.Name)
	}

	if isFullyDeleted(r) {
		return mapGetError(pgx.ErrNoRows, gvk, key.Name)
	}

	return populateObject(obj, r, gvk)
}

func (c *pgClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	listOpts := client.ListOptions{}
	for _, o := range opts {
		o.ApplyToList(&listOpts)
	}

	listGVK, err := resolveGVK(c.scheme, list)
	if err != nil {
		return err
	}
	itemGVK := itemGVKFromListGVK(listGVK)
	gvkStr := gvkToString(itemGVK)

	poolConn, err := c.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer poolConn.Release()
	conn := poolConn.Conn()

	filter, err := buildListFilter(listOpts)
	if err != nil {
		return err
	}

	result, err := reader.List(ctx, conn, gvkStr, filter)
	if err != nil {
		return err
	}

	var items []client.Object
	for _, r := range result.Resources {
		obj, err := resourceToObject(r, c.scheme)
		if err != nil {
			return err
		}
		items = append(items, obj)
	}

	if err := setListItems(list, items); err != nil {
		return err
	}

	list.SetResourceVersion(result.ResourceVersion.String())
	if listOpts.Limit > 0 && int64(len(result.Resources)) == listOpts.Limit {
		offset, _ := decodeContinue(listOpts.Continue)
		list.SetContinue(encodeContinue(offset + listOpts.Limit))
	}
	return nil
}

func (c *pgClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	createOpts := client.CreateOptions{}
	for _, o := range opts {
		o.ApplyToCreate(&createOpts)
	}
	if err := rejectDryRun(createOpts.DryRun); err != nil {
		return err
	}
	if obj.GetName() == "" && obj.GetGenerateName() != "" {
		return errGenerateNameNotSupported
	}

	gvk, err := resolveGVK(c.scheme, obj)
	if err != nil {
		return err
	}
	gvkStr := gvkToString(gvk)

	obj.SetGeneration(1)

	spec, status, err := extractSpecStatus(obj)
	if err != nil {
		return err
	}
	metadata, err := extractMetadata(obj)
	if err != nil {
		return err
	}

	ns, name := obj.GetNamespace(), obj.GetName()

	poolConn, err := c.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer poolConn.Release()
	conn := poolConn.Conn()

	w := writer.New(conn, nil)
	result, err := w.Write(ctx, model.WriteRequest{
		GVK:             gvkStr,
		Namespace:       ns,
		Name:            name,
		Spec:            spec,
		Status:          status,
		Metadata:        metadata,
		ExpectedVersion: 0,
	})
	if err != nil {
		r, wErr := mapWriteError(ctx, w, err, gvk, name, 0)
		if wErr != nil {
			return wErr
		}
		if r != nil {
			return populateObject(obj, *r, gvk)
		}
		return nil
	}

	obj.SetUID(uidFromUUID(result.UID))
	obj.SetResourceVersion(strconv.FormatInt(result.ObjectVersion, 10))
	obj.SetCreationTimestamp(metav1.Now())
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	return nil
}

func (c *pgClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	updateOpts := client.UpdateOptions{}
	for _, o := range opts {
		o.ApplyToUpdate(&updateOpts)
	}
	if err := rejectDryRun(updateOpts.DryRun); err != nil {
		return err
	}

	gvk, err := resolveGVK(c.scheme, obj)
	if err != nil {
		return err
	}
	gvkStr := gvkToString(gvk)

	expectedVersion, err := parseResourceVersion(obj)
	if err != nil {
		return fmt.Errorf("parse resource version: %w", err)
	}
	expectedUID, err := parseObjectUID(obj)
	if err != nil {
		return fmt.Errorf("parse UID: %w", err)
	}
	if expectedVersion > 0 && expectedUID == nil {
		return apierrors.NewBadRequest("metadata.uid is required for updates")
	}

	ns, name := obj.GetNamespace(), obj.GetName()

	poolConn, err := c.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer poolConn.Release()
	conn := poolConn.Conn()

	current, err := readCurrentResource(ctx, conn, gvkStr, ns, name)
	if err != nil {
		return err
	}
	if current == nil {
		return apierrors.NewNotFound(groupResource(gvk), name)
	}
	if expectedUID != nil && current.UID != *expectedUID {
		return apierrors.NewConflict(groupResource(gvk), name,
			fmt.Errorf("object UID changed from %s to %s", *expectedUID, current.UID))
	}

	newSpec, err := extractSpec(obj)
	if err != nil {
		return err
	}

	if !writer.JSONEqual(current.Spec, newSpec) {
		currentGen := currentGeneration(current.Metadata)
		obj.SetGeneration(currentGen + 1)
	}

	metadata, err := extractMetadata(obj)
	if err != nil {
		return err
	}

	w := writer.New(conn, nil)
	result, err := w.WriteObject(ctx, model.ObjectWriteRequest{
		GVK:               gvkStr,
		Namespace:         ns,
		Name:              name,
		Spec:              newSpec,
		Metadata:          metadata,
		DeletionTimestamp: current.DeletionTimestamp,
		ExpectedVersion:   expectedVersion,
		ExpectedUID:       expectedUID,
	})
	if err != nil {
		r, wErr := mapWriteError(ctx, w, err, gvk, name, 0)
		if wErr != nil {
			return wErr
		}
		if r != nil {
			return populateObject(obj, *r, gvk)
		}
		return nil
	}

	obj.SetResourceVersion(strconv.FormatInt(result.ObjectVersion, 10))
	return nil
}

func (c *pgClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	deleteOpts := client.DeleteOptions{}
	for _, o := range opts {
		o.ApplyToDelete(&deleteOpts)
	}
	if err := rejectUnsupportedDeleteOpts(deleteOpts); err != nil {
		return err
	}

	gvk, err := resolveGVK(c.scheme, obj)
	if err != nil {
		return err
	}
	gvkStr := gvkToString(gvk)

	ns, name := obj.GetNamespace(), obj.GetName()

	poolConn, err := c.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer poolConn.Release()
	conn := poolConn.Conn()

	expectedVersion, err := parseResourceVersion(obj)
	if err != nil {
		return fmt.Errorf("parse resource version: %w", err)
	}
	expectedUID, err := parseObjectUID(obj)
	if err != nil {
		return fmt.Errorf("parse UID: %w", err)
	}

	if expectedVersion > 0 && expectedUID == nil {
		return apierrors.NewBadRequest("metadata.uid is required when deleting with resourceVersion")
	}

	current, err := readCurrentResource(ctx, conn, gvkStr, ns, name)
	if err != nil {
		return err
	}
	if current == nil {
		return apierrors.NewNotFound(groupResource(gvk), name)
	}
	if expectedUID != nil && current.UID != *expectedUID {
		return apierrors.NewConflict(groupResource(gvk), name,
			fmt.Errorf("object UID changed from %s to %s", *expectedUID, current.UID))
	}

	// Like the Kubernetes apiserver, the caller's object only supplies
	// preconditions (UID, resourceVersion); the stored row is what gets marked
	// deleted. The write is version-checked against that row.
	if expectedVersion == 0 {
		expectedVersion = current.ObjectVersion
	}
	expectedUID = &current.UID
	metadata := current.Metadata

	deletionTS := current.DeletionTimestamp
	if deletionTS == nil {
		now := time.Now()
		deletionTS = &now
		// Like the Kubernetes apiserver, starting a delete bumps generation so
		// generation-based predicates see it.
		if gen := currentGeneration(metadata); gen > 0 {
			metadata, err = setGeneration(metadata, gen+1)
			if err != nil {
				return fmt.Errorf("bump generation: %w", err)
			}
		}
	}

	w := writer.New(conn, nil)
	_, err = w.Write(ctx, model.WriteRequest{
		GVK:               gvkStr,
		Namespace:         ns,
		Name:              name,
		Spec:              current.Spec,
		Status:            current.Status,
		Metadata:          metadata,
		DeletionTimestamp: deletionTS,
		ExpectedVersion:   expectedVersion,
		ExpectedUID:       expectedUID,
	})
	if err != nil {
		_, wErr := mapWriteError(ctx, w, err, gvk, name, 0)
		return wErr
	}

	return nil
}

// Apply is not implemented; server-side apply requires an apiserver.
func (c *pgClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	return apierrors.NewMethodNotSupported(schema.GroupResource{}, "apply")
}

func (c *pgClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return apierrors.NewMethodNotSupported(schema.GroupResource{}, "patch")
}

func (c *pgClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	return apierrors.NewMethodNotSupported(schema.GroupResource{}, "deleteAllOf")
}

func (c *pgClient) Status() client.SubResourceWriter {
	return &pgStatusWriter{c: c}
}

func (c *pgClient) SubResource(subResource string) client.SubResourceClient {
	if subResource == "status" {
		return &pgSubResourceClient{statusWriter: &pgStatusWriter{c: c}}
	}
	return &pgSubResourceClient{}
}

func (c *pgClient) Scheme() *runtime.Scheme {
	return c.scheme
}

func (c *pgClient) RESTMapper() meta.RESTMapper {
	return c.restMapper
}

func (c *pgClient) GroupVersionKindFor(obj runtime.Object) (schema.GroupVersionKind, error) {
	return resolveGVK(c.scheme, obj)
}

func (c *pgClient) IsObjectNamespaced(obj runtime.Object) (bool, error) {
	gvk, err := resolveGVK(c.scheme, obj)
	if err != nil {
		return true, err
	}
	mapping, err := c.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return true, nil
	}
	return mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// pgStatusWriter implements client.SubResourceWriter for status updates.
type pgStatusWriter struct {
	c *pgClient
}

func (sw *pgStatusWriter) Create(
	ctx context.Context, obj client.Object, subResource client.Object,
	opts ...client.SubResourceCreateOption,
) error {
	return apierrors.NewMethodNotSupported(schema.GroupResource{}, "status create")
}

func (sw *pgStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	updateOpts := client.SubResourceUpdateOptions{}
	for _, o := range opts {
		o.ApplyToSubResourceUpdate(&updateOpts)
	}
	if err := rejectDryRun(updateOpts.DryRun); err != nil {
		return err
	}

	gvk, err := resolveGVK(sw.c.scheme, obj)
	if err != nil {
		return err
	}
	gvkStr := gvkToString(gvk)

	expectedVersion, err := parseResourceVersion(obj)
	if err != nil {
		return fmt.Errorf("parse resource version: %w", err)
	}
	expectedUID, err := parseObjectUID(obj)
	if err != nil {
		return fmt.Errorf("parse UID: %w", err)
	}
	if expectedUID == nil {
		return apierrors.NewBadRequest("metadata.uid is required for status updates")
	}

	status, err := extractStatus(obj)
	if err != nil {
		return err
	}

	ns, name := obj.GetNamespace(), obj.GetName()

	poolConn, err := sw.c.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer poolConn.Release()
	conn := poolConn.Conn()

	w := writer.New(conn, nil)
	result, err := w.WriteStatus(ctx, model.StatusWriteRequest{
		GVK:             gvkStr,
		Namespace:       ns,
		Name:            name,
		Status:          status,
		ExpectedVersion: expectedVersion,
		ExpectedUID:     expectedUID,
	})
	if err != nil {
		r, wErr := mapWriteError(ctx, w, err, gvk, name, 0)
		if wErr != nil {
			return wErr
		}
		if r != nil {
			return populateObject(obj, *r, gvk)
		}
		return nil
	}

	obj.SetResourceVersion(strconv.FormatInt(result.ObjectVersion, 10))
	return nil
}

// Apply is not implemented; server-side apply requires an apiserver.
func (sw *pgStatusWriter) Apply(
	ctx context.Context, obj runtime.ApplyConfiguration,
	opts ...client.SubResourceApplyOption,
) error {
	return apierrors.NewMethodNotSupported(schema.GroupResource{}, "status apply")
}

func (sw *pgStatusWriter) Patch(
	ctx context.Context, obj client.Object, patch client.Patch,
	opts ...client.SubResourcePatchOption,
) error {
	return apierrors.NewMethodNotSupported(schema.GroupResource{}, "status patch")
}

// pgSubResourceClient wraps status writer as a SubResourceClient.
type pgSubResourceClient struct {
	statusWriter *pgStatusWriter
}

func (src *pgSubResourceClient) Get(
	ctx context.Context, obj client.Object, subResource client.Object,
	opts ...client.SubResourceGetOption,
) error {
	return apierrors.NewMethodNotSupported(schema.GroupResource{}, "subresource get")
}

func (src *pgSubResourceClient) Create(
	ctx context.Context, obj client.Object, subResource client.Object,
	opts ...client.SubResourceCreateOption,
) error {
	if src.statusWriter != nil {
		return src.statusWriter.Create(ctx, obj, subResource, opts...)
	}
	return apierrors.NewMethodNotSupported(schema.GroupResource{}, "subresource create")
}

func (src *pgSubResourceClient) Update(
	ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption,
) error {
	if src.statusWriter != nil {
		return src.statusWriter.Update(ctx, obj, opts...)
	}
	return apierrors.NewMethodNotSupported(schema.GroupResource{}, "subresource update")
}

// Apply is not implemented; server-side apply requires an apiserver.
func (src *pgSubResourceClient) Apply(
	ctx context.Context, obj runtime.ApplyConfiguration,
	opts ...client.SubResourceApplyOption,
) error {
	if src.statusWriter != nil {
		return src.statusWriter.Apply(ctx, obj, opts...)
	}
	return apierrors.NewMethodNotSupported(schema.GroupResource{}, "subresource apply")
}

func (src *pgSubResourceClient) Patch(
	ctx context.Context, obj client.Object, patch client.Patch,
	opts ...client.SubResourcePatchOption,
) error {
	if src.statusWriter != nil {
		return src.statusWriter.Patch(ctx, obj, patch, opts...)
	}
	return apierrors.NewMethodNotSupported(schema.GroupResource{}, "subresource patch")
}

func readCurrentResource(ctx context.Context, conn *pgx.Conn, gvk, ns, name string) (*model.Resource, error) {
	var r model.Resource
	err := conn.QueryRow(ctx, `
		SELECT gvk, namespace, name, uid, txid_stamp::text::bigint,
		       object_version, spec, status, metadata,
		       deletion_timestamp, created_at, updated_at
		FROM kubernetes_resources
		WHERE gvk = $1 AND namespace = $2 AND name = $3
		  AND (deletion_timestamp IS NULL
		       OR (metadata->'finalizers' IS NOT NULL
		           AND metadata->'finalizers' != '[]'::jsonb))`,
		gvk, ns, name,
	).Scan(
		&r.GVK, &r.Namespace, &r.Name, &r.UID,
		&r.TxidStamp, &r.ObjectVersion, &r.Spec, &r.Status,
		&r.Metadata, &r.DeletionTimestamp, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("read current resource: %w", err)
	}
	return &r, nil
}

func currentGeneration(metadata json.RawMessage) int64 {
	if len(metadata) == 0 {
		return 0
	}
	var sm storedMetadata
	if err := json.Unmarshal(metadata, &sm); err != nil {
		return 0
	}
	return sm.Generation
}

// setGeneration sets metadata.generation. Other metadata keys are preserved
// as given.
func setGeneration(metadata json.RawMessage, gen int64) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &m); err != nil {
		return nil, err
	}
	next, err := json.Marshal(gen)
	if err != nil {
		return nil, err
	}
	m["generation"] = next
	return json.Marshal(m)
}

func uidFromUUID(u [16]byte) apitypes.UID {
	return apitypes.UID(fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]))
}

func parseObjectUID(obj client.Object) (*uuid.UUID, error) {
	if obj.GetUID() == "" {
		return nil, nil
	}
	u, err := uuid.Parse(string(obj.GetUID()))
	if err != nil {
		return nil, err
	}
	return &u, nil
}

type continueToken struct {
	Offset int64 `json:"offset"`
}

func decodeContinue(token string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	data, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return 0, fmt.Errorf("pgruntime: invalid continue token: %w", err)
	}
	var ct continueToken
	if err := json.Unmarshal(data, &ct); err != nil {
		return 0, fmt.Errorf("pgruntime: invalid continue token: %w", err)
	}
	return ct.Offset, nil
}

func encodeContinue(offset int64) string {
	data, _ := json.Marshal(continueToken{Offset: offset})
	return base64.StdEncoding.EncodeToString(data)
}

func buildListFilter(listOpts client.ListOptions) (*reader.ListFilter, error) {
	var f reader.ListFilter

	// $1 = gvk, so extra params start at $2
	paramIdx := 2

	if listOpts.Namespace != "" {
		f.WhereClauses = append(f.WhereClauses, fmt.Sprintf("namespace = $%d", paramIdx))
		f.WhereArgs = append(f.WhereArgs, listOpts.Namespace)
		paramIdx++
	}

	if listOpts.FieldSelector != nil && !listOpts.FieldSelector.Empty() {
		clauses, args, err := buildFieldSelectorFilter(listOpts.FieldSelector, paramIdx)
		if err != nil {
			return nil, err
		}
		f.WhereClauses = append(f.WhereClauses, clauses...)
		f.WhereArgs = append(f.WhereArgs, args...)
		paramIdx += len(args)
	}

	if listOpts.LabelSelector != nil && !listOpts.LabelSelector.Empty() {
		clauses, args, err := buildLabelSelectorFilter(listOpts.LabelSelector, paramIdx)
		if err != nil {
			return nil, err
		}
		f.WhereClauses = append(f.WhereClauses, clauses...)
		f.WhereArgs = append(f.WhereArgs, args...)
	}

	if listOpts.Limit > 0 {
		f.Limit = listOpts.Limit
		offset, err := decodeContinue(listOpts.Continue)
		if err != nil {
			return nil, err
		}
		f.Offset = offset
	}

	if f.Limit == 0 && len(f.WhereClauses) == 0 {
		return nil, nil
	}
	return &f, nil
}

var _ client.Client = (*pgClient)(nil)
