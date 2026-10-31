package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// upsertRenderedResource persists a rendered resource as a desired-state
// snapshot. The database-managed identity is applied only to the copy sent to
// PostgreSQL, so it cannot leak into the management-cluster desire payload.
func upsertRenderedResource(
	ctx context.Context,
	c client.Client,
	desired client.Object,
	newObject func() client.Object,
) error {
	key := client.ObjectKeyFromObject(desired)
	current := newObject()
	if err := c.Get(ctx, key, current); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get rendered resource %s/%s: %w", key.Namespace, key.Name, err)
		}

		snapshot, ok := desired.DeepCopyObject().(client.Object)
		if !ok {
			return fmt.Errorf("deep-copy rendered resource %T: result is not a client object", desired)
		}
		if err := c.Create(ctx, snapshot); err != nil {
			return fmt.Errorf("create rendered resource %s/%s: %w", key.Namespace, key.Name, err)
		}
		return nil
	}

	snapshot, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("deep-copy rendered resource %T: result is not a client object", desired)
	}
	snapshot.SetUID(current.GetUID())
	snapshot.SetResourceVersion(current.GetResourceVersion())
	if err := c.Update(ctx, snapshot); err != nil {
		return fmt.Errorf("update rendered resource %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}

func deleteRenderedResource(ctx context.Context, c client.Client, object client.Object) error {
	if err := c.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
		key := client.ObjectKeyFromObject(object)
		return fmt.Errorf("delete rendered resource %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}
