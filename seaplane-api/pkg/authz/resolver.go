package authz

import (
	"context"
	"errors"
	"fmt"
)

// Resolver freezes authorization inputs for one request.
type Resolver interface {
	Prepare(context.Context, Identity, RequestContext) (*Prepared, error)
}

// Prepare binds the caller's stored grants for one resource request.
func (m *Service) Prepare(ctx context.Context, id Identity, req RequestContext) (*Prepared, error) {
	store := m.store(id.AccountID)
	if store == nil {
		return nil, newFailure(StageResolution, fmt.Errorf("invalid authorization account"))
	}
	prepared, err := store.Prepare(ctx, id, req)
	var failure *Failure
	if err != nil && !errors.As(err, &failure) {
		return nil, newFailure(StageResolution, err)
	}
	return prepared, err
}
