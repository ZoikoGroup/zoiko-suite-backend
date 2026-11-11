package registry

import (
	"context"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// FXStore is the data-access contract for REF-03 exchange rates (migration 000022). Asserted by the service like
// PartyStore: a store without it answers ErrServiceUnavailable. Every method runs in the caller's tenant under
// row-level security.
type FXStore interface {
	CreateFXSource(ctx context.Context, req domain.CreateFXSourceRequest, actor string) (*domain.FXSource, error)
	WithdrawFXSource(ctx context.Context, sourceID string, req domain.WithdrawFXSourceRequest, actor string, allowUnversioned bool) (*domain.FXSource, error)
	ListFXSources(ctx context.Context) ([]domain.FXSource, error)

	// IngestFXObservation records one observation under the source's row lock, runs the validation checks (duplicate,
	// outlier against the last accepted rate of the pair and type) and retains their results. A replay of the same
	// observation returns the first; the same key with a different rate is ErrConflict. A manual or outlying rate
	// stays OBSERVED and needs independent approval; the rest are VALIDATED.
	IngestFXObservation(ctx context.Context, req domain.IngestFXObservationRequest, actor string) (*FXIngestOutcome, error)
	// SupersedeFXObservation replaces an observation that is not yet in a rate set with a corrected one for the same
	// pair, type and market date. A rate in a set is corrected by a superseding set, never by editing it.
	SupersedeFXObservation(ctx context.Context, observationID string, req domain.SupersedeFXObservationRequest, actor string) (*FXIngestOutcome, error)

	// ApproveFXRateSet creates an approved set from observations. A manual or outlying observation needs an override
	// reason and an approver other than its ingester (ErrSelfApproval). SupersedesSetID makes it a correction.
	ApproveFXRateSet(ctx context.Context, req domain.ApproveFXRateSetRequest, actor string) (*domain.FXRateSet, error)
	// CloseFXRateSet certifies an APPROVED set; it is then immutable.
	CloseFXRateSet(ctx context.Context, setID string, req domain.CloseFXRateSetRequest, actor string) (*domain.FXRateSet, error)
	GetFXRateSet(ctx context.Context, setID string) (*domain.FXRateSet, error)

	// GetApprovedFXComponent returns the approved rate of a pair for a type, date and set name, with where it came
	// from. knownAt zero means now; otherwise the rate set live at that instant (a later correction is not seen).
	// (nil, nil) when there is none.
	GetApprovedFXComponent(ctx context.Context, base, quote string, typ domain.FXRateType, marketDate time.Time, setName string, knownAt time.Time) (*domain.FXComponent, error)
	ListFXRatesBySource(ctx context.Context, sourceID string, from, to time.Time, limit int, after string) ([]domain.FXObservation, string, error)
}

// FXIngestOutcome is an observation after ingestion, or a replacement.
type FXIngestOutcome struct {
	Observation domain.FXObservation  `json:"observation"`
	Superseded  *domain.FXObservation `json:"superseded,omitempty"`
	Replayed    bool                  `json:"-"`
}
