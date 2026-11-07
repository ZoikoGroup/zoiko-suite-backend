package registry

import (
	"context"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// ReferenceStore is the data-access contract for MDM-04 reference releases (migration 000026). Asserted by the service like
// PartyStore. Every method runs in the caller's tenant under row-level security.
type ReferenceStore interface {
	CreateReferenceSet(ctx context.Context, req domain.CreateReferenceSetRequest, actor string) (*domain.ReferenceSet, error)
	ListReferenceSets(ctx context.Context) ([]domain.ReferenceSet, error)
	// GetReferenceSet returns the set; (nil, nil) when absent.
	GetReferenceSet(ctx context.Context, setCode string) (*domain.ReferenceSet, error)

	// StageReferenceRelease records a release and its entries atomically. It governs nothing until published. The entries are
	// checked against the set's published state (ErrInvalidInput naming the code), and the effective date must follow every
	// published release (ErrStateConflict). A version is used once (ErrConflict).
	StageReferenceRelease(ctx context.Context, setCode string, req domain.StageReferenceReleaseRequest, actor string) (*domain.ReferenceRelease, error)
	// PublishReferenceRelease puts a staged release in force from its effective date. Its stager cannot publish it
	// (ErrSelfApproval). The entries are checked again against what has been published since staging.
	PublishReferenceRelease(ctx context.Context, setCode, version string, req domain.PublishReferenceReleaseRequest, actor string) (*domain.ReferenceRelease, error)
	ListReferenceReleases(ctx context.Context, setCode string) ([]domain.ReferenceRelease, error)

	// ResolveReference returns a code as it stood on the date, or under a pinned release; ErrNotFound when it had not been
	// added, or the set or release does not exist.
	ResolveReference(ctx context.Context, q domain.ReferenceQuery) (*domain.ReferenceResolution, error)
}
