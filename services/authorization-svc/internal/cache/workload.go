package cache

import (
	"context"

	"zoiko.io/authorization-svc/internal/domain"
)

// Workload-binding capabilities (Governance audit N-09). The handler finds
// them by type assertion, so the wrapper forwards each one; an inner store
// without it answers ErrStoreUnavailable, which the handler turns into 503.

type workloadAdmin interface {
	SaveWorkloadBinding(ctx context.Context, wb domain.WorkloadBinding, event domain.OutboxMessage) (*domain.WorkloadBinding, error)
	SetWorkloadBindingActive(ctx context.Context, workloadID, tenantID string, active bool, event domain.OutboxMessage) (*domain.WorkloadBinding, error)
	ListWorkloadBindings(ctx context.Context, tenantID string) ([]domain.WorkloadBinding, error)
	IsBoundWorkload(ctx context.Context, principalID string) (bool, error)
}

const nsWorkload = "workload"

func (s *Store) SaveWorkloadBinding(ctx context.Context, wb domain.WorkloadBinding, event domain.OutboxMessage) (*domain.WorkloadBinding, error) {
	f, ok := s.inner.(workloadAdmin)
	if !ok {
		return nil, domain.ErrStoreUnavailable
	}
	out, err := f.SaveWorkloadBinding(ctx, wb, event)
	if err == nil {
		s.invalidate(nsWorkload, "")
	}
	return out, err
}

func (s *Store) SetWorkloadBindingActive(ctx context.Context, workloadID, tenantID string, active bool, event domain.OutboxMessage) (*domain.WorkloadBinding, error) {
	f, ok := s.inner.(workloadAdmin)
	if !ok {
		return nil, domain.ErrStoreUnavailable
	}
	out, err := f.SetWorkloadBindingActive(ctx, workloadID, tenantID, active, event)
	if err == nil {
		s.invalidate(nsWorkload, "")
	}
	return out, err
}

func (s *Store) ListWorkloadBindings(ctx context.Context, tenantID string) ([]domain.WorkloadBinding, error) {
	f, ok := s.inner.(workloadAdmin)
	if !ok {
		return nil, domain.ErrStoreUnavailable
	}
	return f.ListWorkloadBindings(ctx, tenantID)
}

// IsBoundWorkload is cached: it is asked on every decision and the answer
// almost never changes. A binding written through this replica invalidates it
// at once; one written elsewhere is seen within the TTL — and FindWorkloadBinding,
// which decides what a known workload may do, is not cached at all.
func (s *Store) IsBoundWorkload(ctx context.Context, principalID string) (bool, error) {
	f, ok := s.inner.(workloadAdmin)
	if !ok {
		return false, nil
	}
	k := s.key(nsWorkload, "", principalID)
	if v, ok := s.load(k); ok {
		return v.(bool), nil
	}
	bound, err := f.IsBoundWorkload(ctx, principalID)
	if err != nil {
		return false, err
	}
	s.save(k, bound)
	return bound, nil
}
