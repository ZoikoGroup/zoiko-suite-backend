package registry

// MDM-04 Reference releases: CreateReferenceSet, StageReferenceRelease, PublishReferenceRelease and the reads
// ListReferenceSets, ListReferenceReleases and ResolveReference (ZS-SVC-U-001 s15-16, s19).
//
// Reference sets are tenant-wide, so every command is authorized at the tenant. Releasing is maker-checker: the principal who
// staged a release does not publish it. Reading a code is never gated: it answers from published releases only.

import (
	"context"
	"fmt"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *Service) referenceStore() (ReferenceStore, error) {
	rs, ok := s.store.(ReferenceStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return rs, nil
}

func (s *Service) referenceCommand(ctx context.Context, action string, validate func() error) (ReferenceStore, error) {
	if err := validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "reference", action); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	return s.referenceStore()
}

// CreateReferenceSet defines a reference set.
func (s *Service) CreateReferenceSet(ctx context.Context, req domain.CreateReferenceSetRequest) (*domain.ReferenceSet, error) {
	rs, err := s.referenceCommand(ctx, "create-set", req.Validate)
	if err != nil {
		return nil, err
	}
	out, err := rs.CreateReferenceSet(ctx, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.CreateReferenceSet: %w", err)
	}
	return out, nil
}

// ListReferenceSets lists the reference sets of the tenant.
func (s *Service) ListReferenceSets(ctx context.Context) ([]domain.ReferenceSet, error) {
	rs, err := s.referenceStore()
	if err != nil {
		return nil, err
	}
	out, err := rs.ListReferenceSets(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.ListReferenceSets: %w", err)
	}
	return out, nil
}

// StageReferenceRelease stages a release.
func (s *Service) StageReferenceRelease(ctx context.Context, setCode string, req domain.StageReferenceReleaseRequest) (*domain.ReferenceRelease, error) {
	rs, err := s.referenceCommand(ctx, "stage-release", req.Validate)
	if err != nil {
		return nil, err
	}
	out, err := rs.StageReferenceRelease(ctx, setCode, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.StageReferenceRelease: %w", err)
	}
	return out, nil
}

// PublishReferenceRelease publishes a staged release.
func (s *Service) PublishReferenceRelease(ctx context.Context, setCode, version string, req domain.PublishReferenceReleaseRequest) (*domain.ReferenceRelease, error) {
	rs, err := s.referenceCommand(ctx, "publish-release", req.Validate)
	if err != nil {
		return nil, err
	}
	out, err := rs.PublishReferenceRelease(ctx, setCode, version, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.PublishReferenceRelease: %w", err)
	}
	return out, nil
}

// ListReferenceReleases lists the releases of a set, staged and published.
func (s *Service) ListReferenceReleases(ctx context.Context, setCode string) ([]domain.ReferenceRelease, error) {
	rs, err := s.referenceStore()
	if err != nil {
		return nil, err
	}
	out, err := rs.ListReferenceReleases(ctx, setCode)
	if err != nil {
		return nil, fmt.Errorf("store.ListReferenceReleases: %w", err)
	}
	return out, nil
}

// ResolveReference is Resolve reference: a code as it stood on a date, or under one exact release.
func (s *Service) ResolveReference(ctx context.Context, q domain.ReferenceQuery) (*domain.ReferenceResolution, error) {
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	rs, err := s.referenceStore()
	if err != nil {
		return nil, err
	}
	if q.AsOf.IsZero() {
		q.AsOf = time.Now().UTC()
	}
	out, err := rs.ResolveReference(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store.ResolveReference: %w", err)
	}
	return out, nil
}
