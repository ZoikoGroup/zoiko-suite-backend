package registry

// REF-03 Foreign Exchange Rates: IngestFXObservation, ApproveFXRateSet, SupersedeRateObservation, CloseRateSet and the
// reads GetRate, GetRateSet, ListRatesBySource, ExplainDerivedCrossRate and GetHistoricalRate (ZS-SVC-I-001 s4.13).
//
// Three rules shape it. A rate is never accepted for a currency the registry does not confirm, and if the registry
// cannot answer the rate is not accepted either. A missing approved rate is an explicit FX_RATE_MISSING, never a
// fallback to an older date, another type or an unapproved observation. And a derived rate is only ever built from
// approved components and says which ones and how, in exact arithmetic.

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *Service) fxStore() (FXStore, error) {
	fs, ok := s.store.(FXStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return fs, nil
}

// checkCurrencies confirms each code with currency-registry-svc, read-only and fail-closed.
func (s *Service) checkCurrencies(ctx context.Context, codes ...string) error {
	if s.currencies == nil {
		return fmt.Errorf("%w: no currency registry is configured", ErrServiceUnavailable)
	}
	for _, c := range codes {
		res, err := s.currencies.Validate(ctx, c)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrServiceUnavailable, err)
		}
		if !res.Supported {
			return fmt.Errorf("%w: currency %s is not known to the currency registry (%s)", ErrReferenceInvalid, c, res.Reason)
		}
	}
	return nil
}

// CreateFXSource registers and approves a source of rates.
func (s *Service) CreateFXSource(ctx context.Context, req domain.CreateFXSourceRequest) (*domain.FXSource, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "fx.source", "manage"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	fs, err := s.fxStore()
	if err != nil {
		return nil, err
	}
	out, err := fs.CreateFXSource(ctx, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.CreateFXSource: %w", err)
	}
	return out, nil
}

// WithdrawFXSource withdraws a source: no new rates from it, existing ones stay.
func (s *Service) WithdrawFXSource(ctx context.Context, sourceID string, req domain.WithdrawFXSourceRequest) (*domain.FXSource, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "fx.source", "manage"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	fs, err := s.fxStore()
	if err != nil {
		return nil, err
	}
	out, err := fs.WithdrawFXSource(ctx, sourceID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.WithdrawFXSource: %w", err)
	}
	return out, nil
}

// ListFXSources lists the tenant's sources.
func (s *Service) ListFXSources(ctx context.Context) ([]domain.FXSource, error) {
	fs, err := s.fxStore()
	if err != nil {
		return nil, err
	}
	out, err := fs.ListFXSources(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.ListFXSources: %w", err)
	}
	return out, nil
}

// IngestFXObservation is IngestFXObservation.
func (s *Service) IngestFXObservation(ctx context.Context, req domain.IngestFXObservationRequest) (*FXIngestOutcome, error) {
	if err := req.Validate(time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "fx.observation", "ingest"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	if err := s.checkCurrencies(ctx, req.BaseCurrency, req.QuoteCurrency); err != nil {
		return nil, err
	}
	fs, err := s.fxStore()
	if err != nil {
		return nil, err
	}
	out, err := fs.IngestFXObservation(ctx, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.IngestFXObservation: %w", err)
	}
	return out, nil
}

// SupersedeFXObservation is SupersedeRateObservation.
func (s *Service) SupersedeFXObservation(ctx context.Context, observationID string, req domain.SupersedeFXObservationRequest) (*FXIngestOutcome, error) {
	if err := req.Validate(time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "fx.observation", "supersede"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	if err := s.checkCurrencies(ctx, req.BaseCurrency, req.QuoteCurrency); err != nil {
		return nil, err
	}
	fs, err := s.fxStore()
	if err != nil {
		return nil, err
	}
	out, err := fs.SupersedeFXObservation(ctx, observationID, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.SupersedeFXObservation: %w", err)
	}
	return out, nil
}

// ApproveFXRateSet is ApproveFXRateSet.
func (s *Service) ApproveFXRateSet(ctx context.Context, req domain.ApproveFXRateSetRequest) (*domain.FXRateSet, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "fx.rate-set", "approve"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	fs, err := s.fxStore()
	if err != nil {
		return nil, err
	}
	out, err := fs.ApproveFXRateSet(ctx, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.ApproveFXRateSet: %w", err)
	}
	return out, nil
}

// CloseFXRateSet is CloseRateSet.
func (s *Service) CloseFXRateSet(ctx context.Context, setID string, req domain.CloseFXRateSetRequest) (*domain.FXRateSet, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "fx.rate-set", "close"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	fs, err := s.fxStore()
	if err != nil {
		return nil, err
	}
	out, err := fs.CloseFXRateSet(ctx, setID, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.CloseFXRateSet: %w", err)
	}
	return out, nil
}

// GetFXRateSet is GetRateSet.
func (s *Service) GetFXRateSet(ctx context.Context, setID string) (*domain.FXRateSet, error) {
	fs, err := s.fxStore()
	if err != nil {
		return nil, err
	}
	out, err := fs.GetFXRateSet(ctx, setID)
	if err != nil {
		return nil, fmt.Errorf("store.GetFXRateSet: %w", err)
	}
	if out == nil {
		return nil, ErrNotFound
	}
	return out, nil
}

// ListFXRatesBySource is ListRatesBySource: a source's observations by market date, newest first.
func (s *Service) ListFXRatesBySource(ctx context.Context, sourceID string, from, to time.Time, limit int, after string) ([]domain.FXObservation, string, error) {
	if !to.IsZero() && !from.IsZero() && to.Before(from) {
		return nil, "", fmt.Errorf("%w: to is before from", ErrInvalidInput)
	}
	fs, err := s.fxStore()
	if err != nil {
		return nil, "", err
	}
	out, next, err := fs.ListFXRatesBySource(ctx, sourceID, from, to, limit, after)
	if err != nil {
		return nil, "", fmt.Errorf("store.ListFXRatesBySource: %w", err)
	}
	return out, next, nil
}

// FXRateQuery names a rate: the pair, the type, the market date, the set and, for history, what was known when.
type FXRateQuery struct {
	Base, Quote string
	Type        domain.FXRateType
	MarketDate  time.Time
	SetName     string
	// Via is the pivot currency a cross rate is derived through; empty means the default.
	Via string
	// KnownAt is zero for now. A non-zero instant answers with the rate set that was live then.
	KnownAt time.Time
}

func (q *FXRateQuery) validate() error {
	if len(q.Base) != 3 || len(q.Quote) != 3 || q.Base == q.Quote {
		return fmt.Errorf("%w: base and quote must be two different three-letter currency codes", ErrInvalidInput)
	}
	if !q.Type.Valid() {
		return fmt.Errorf("%w: rate_type is required", ErrInvalidInput)
	}
	if q.MarketDate.IsZero() {
		return fmt.Errorf("%w: market_date is required", ErrInvalidInput)
	}
	if q.SetName == "" {
		q.SetName = "DEFAULT"
	}
	return nil
}

func component(c *domain.FXComponent, dir string) domain.FXComponent {
	cp := *c
	cp.Direction = dir
	return cp
}

// GetFXRate is GetRate: the approved direct rate for the pair, or ErrRateMissing. Nothing is inverted, crossed or
// substituted here; ExplainDerivedCrossRate does that, openly.
func (s *Service) GetFXRate(ctx context.Context, q FXRateQuery) (*domain.FXRate, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	fs, err := s.fxStore()
	if err != nil {
		return nil, err
	}
	c, err := fs.GetApprovedFXComponent(ctx, q.Base, q.Quote, q.Type, q.MarketDate, q.SetName, q.KnownAt)
	if err != nil {
		return nil, fmt.Errorf("store.GetApprovedFXComponent: %w", err)
	}
	if c == nil {
		return nil, ErrRateMissing
	}
	return &domain.FXRate{BaseCurrency: q.Base, QuoteCurrency: q.Quote, RateType: q.Type, MarketDate: c.MarketDate, Rate: c.Rate,
		Method: domain.FXDirect, Formula: fmt.Sprintf("%s/%s as approved", q.Base, q.Quote), Components: []domain.FXComponent{component(c, domain.FXDirect)}}, nil
}

// GetFXHistoricalRate is GetHistoricalRate: the rate as it was known at KnownAt, so a rate a later correction
// replaced still answers a question about what was in force then.
func (s *Service) GetFXHistoricalRate(ctx context.Context, q FXRateQuery) (*domain.FXRate, error) {
	if q.KnownAt.IsZero() {
		return nil, fmt.Errorf("%w: known_at is required for a historical rate", ErrInvalidInput)
	}
	return s.GetFXRate(ctx, q)
}

// leg finds the approved rate that takes from to to: directly, or by inverting the opposite pair.
func (s *Service) leg(ctx context.Context, fs FXStore, q FXRateQuery, from, to string) (*big.Rat, *domain.FXComponent, string, error) {
	c, err := fs.GetApprovedFXComponent(ctx, from, to, q.Type, q.MarketDate, q.SetName, q.KnownAt)
	if err != nil {
		return nil, nil, "", err
	}
	if c != nil {
		r, perr := domain.ParseRate(c.Rate)
		if perr != nil {
			return nil, nil, "", perr
		}
		return r, c, domain.FXDirect, nil
	}
	c, err = fs.GetApprovedFXComponent(ctx, to, from, q.Type, q.MarketDate, q.SetName, q.KnownAt)
	if err != nil || c == nil {
		return nil, nil, "", err
	}
	r, perr := domain.ParseRate(c.Rate)
	if perr != nil {
		return nil, nil, "", perr
	}
	return domain.InvertRate(r), c, "INVERTED", nil
}

// ExplainDerivedCrossRate is ExplainDerivedCrossRate: the rate, how it was reached and from which approved
// observations. It tries the direct rate, then the inverse, then a cross through the pivot currency; nothing else, so
// it never papers over a missing rate with an older or unapproved one.
func (s *Service) ExplainDerivedCrossRate(ctx context.Context, q FXRateQuery) (*domain.FXRate, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	fs, err := s.fxStore()
	if err != nil {
		return nil, err
	}
	out := &domain.FXRate{BaseCurrency: q.Base, QuoteCurrency: q.Quote, RateType: q.Type, MarketDate: q.MarketDate.UTC().Format("2006-01-02")}

	r, c, dir, err := s.leg(ctx, fs, q, q.Base, q.Quote)
	if err != nil {
		return nil, fmt.Errorf("store.GetApprovedFXComponent: %w", err)
	}
	if c != nil {
		out.Components = []domain.FXComponent{component(c, dir)}
		if dir == domain.FXDirect {
			out.Method, out.Rate, out.Formula = domain.FXDirect, c.Rate, fmt.Sprintf("%s/%s as approved", q.Base, q.Quote)
			return out, nil
		}
		out.Method, out.Rate = domain.FXInverse, domain.FormatRate(r)
		out.Formula = fmt.Sprintf("%s/%s = 1 / (%s/%s)", q.Base, q.Quote, q.Quote, q.Base)
		out.Rounding, out.Exact = domain.FXRounding, domain.FractionString(r)
		return out, nil
	}

	pivot := q.Via
	if pivot == "" {
		pivot = domain.FXDefaultPivot
	}
	if pivot == q.Base || pivot == q.Quote {
		return nil, ErrRateMissing
	}
	r1, c1, d1, err := s.leg(ctx, fs, q, q.Base, pivot)
	if err != nil {
		return nil, fmt.Errorf("store.GetApprovedFXComponent: %w", err)
	}
	r2, c2, d2, err := s.leg(ctx, fs, q, pivot, q.Quote)
	if err != nil {
		return nil, fmt.Errorf("store.GetApprovedFXComponent: %w", err)
	}
	if c1 == nil || c2 == nil {
		return nil, ErrRateMissing
	}
	rate := new(big.Rat).Mul(r1, r2)
	out.Method, out.Rate = domain.FXCross, domain.FormatRate(rate)
	out.Formula = fmt.Sprintf("%s/%s = %s/%s x %s/%s", q.Base, q.Quote, q.Base, pivot, pivot, q.Quote)
	out.Rounding, out.Exact = domain.FXRounding, domain.FractionString(rate)
	out.Components = []domain.FXComponent{component(c1, d1), component(c2, d2)}
	return out, nil
}
