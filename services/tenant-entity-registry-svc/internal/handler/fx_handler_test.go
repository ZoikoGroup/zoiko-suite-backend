package handler_test

// REF-03 routes. partySvc also implements handler.FXService, so the OpenAPI parity test covers them in both directions.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	fxSourceID = "0190a000-0000-7000-8000-0000000fa001"
	fxObsID    = "0190a000-0000-7000-8000-0000000fa002"
	fxSetID    = "0190a000-0000-7000-8000-0000000fa003"
)

type fxState struct {
	ingest    *registry.FXIngestOutcome
	err       error
	gotIngest domain.IngestFXObservationRequest
	gotApp    domain.ApproveFXRateSetRequest
	gotQuery  registry.FXRateQuery
	method    string
	gotFrom   time.Time
	gotTo     time.Time
	gotLimit  int
	gotCursor string
	gotID     string
}

var fxStates = map[*partySvc]*fxState{}

func (p *partySvc) x() *fxState {
	if fxStates[p] == nil {
		fxStates[p] = &fxState{ingest: &registry.FXIngestOutcome{}}
	}
	return fxStates[p]
}

func (p *partySvc) CreateFXSource(context.Context, domain.CreateFXSourceRequest) (*domain.FXSource, error) {
	return &domain.FXSource{}, p.x().err
}
func (p *partySvc) WithdrawFXSource(_ context.Context, id string, _ domain.WithdrawFXSourceRequest) (*domain.FXSource, error) {
	p.x().gotID = id
	return &domain.FXSource{}, p.x().err
}
func (p *partySvc) ListFXSources(context.Context) ([]domain.FXSource, error) {
	return []domain.FXSource{}, p.x().err
}
func (p *partySvc) IngestFXObservation(_ context.Context, req domain.IngestFXObservationRequest) (*registry.FXIngestOutcome, error) {
	p.x().gotIngest = req
	return p.x().ingest, p.x().err
}
func (p *partySvc) SupersedeFXObservation(_ context.Context, id string, _ domain.SupersedeFXObservationRequest) (*registry.FXIngestOutcome, error) {
	p.x().gotID = id
	return p.x().ingest, p.x().err
}
func (p *partySvc) ApproveFXRateSet(_ context.Context, req domain.ApproveFXRateSetRequest) (*domain.FXRateSet, error) {
	p.x().gotApp = req
	return &domain.FXRateSet{}, p.x().err
}
func (p *partySvc) CloseFXRateSet(_ context.Context, id string, _ domain.CloseFXRateSetRequest) (*domain.FXRateSet, error) {
	p.x().gotID = id
	return &domain.FXRateSet{}, p.x().err
}
func (p *partySvc) GetFXRateSet(_ context.Context, id string) (*domain.FXRateSet, error) {
	p.x().gotID = id
	return &domain.FXRateSet{}, p.x().err
}
func (p *partySvc) ListFXRatesBySource(_ context.Context, id string, from, to time.Time, limit int, after string) ([]domain.FXObservation, string, error) {
	p.x().gotID, p.x().gotFrom, p.x().gotTo, p.x().gotLimit, p.x().gotCursor = id, from, to, limit, after
	return []domain.FXObservation{}, "next", p.x().err
}
func (p *partySvc) GetFXRate(_ context.Context, q registry.FXRateQuery) (*domain.FXRate, error) {
	p.x().gotQuery, p.x().method = q, "get"
	return &domain.FXRate{}, p.x().err
}
func (p *partySvc) GetFXHistoricalRate(_ context.Context, q registry.FXRateQuery) (*domain.FXRate, error) {
	p.x().gotQuery, p.x().method = q, "history"
	return &domain.FXRate{}, p.x().err
}
func (p *partySvc) ExplainDerivedCrossRate(_ context.Context, q registry.FXRateQuery) (*domain.FXRate, error) {
	p.x().gotQuery, p.x().method = q, "explain"
	return &domain.FXRate{}, p.x().err
}

const fxObsBody = `{"source_id":"` + fxSourceID + `","base_currency":"EUR","quote_currency":"USD","rate_type":"SPOT","rate":"1.085","market_date":"2026-06-12",` +
	`"observed_at":"2026-06-12T15:00:00Z","payload_ref":"s3://x"}`

func TestFXRoutes_AbsentWhenTheServiceOffersNone(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, do(newRouter(&stubSvc{}), http.MethodPost, "/v1/fx/observations", fxObsBody).Code)
}

func TestFXRoutes_Commands(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)

	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/fx/sources", `{"source_code":"ECB"}`).Code)
	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/fx/sources/"+fxSourceID+":withdraw", `{"expected_version":1,"reason":"r"}`).Code)
	assert.Equal(t, fxSourceID, p.x().gotID)
	assert.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/fx/sources", "").Code)

	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/fx/observations", fxObsBody).Code)
	assert.Equal(t, "1.085", p.x().gotIngest.Rate, "the rate arrives as the decimal string it was sent")
	assert.Equal(t, domain.FXSpot, p.x().gotIngest.RateType)
	assert.NotEmpty(t, p.x().gotIngest.CorrelationID)
	p.x().ingest = &registry.FXIngestOutcome{Replayed: true}
	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/fx/observations", fxObsBody).Code, "a replay is 200")
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/fx/observations", "nope").Code)

	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/fx/observations/"+fxObsID+":supersede", fxObsBody).Code)
	assert.Equal(t, fxObsID, p.x().gotID)

	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/fx/rate-sets", `{"rate_type":"CLOSING","market_date":"2026-06-12","observation_ids":["a"],"evidence_ref":"e"}`).Code)
	assert.Equal(t, domain.FXClosing, p.x().gotApp.RateType)
	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/fx/rate-sets/"+fxSetID+":close", `{"evidence_ref":"e"}`).Code)
	assert.Equal(t, fxSetID, p.x().gotID)
	assert.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/fx/rate-sets/"+fxSetID, "").Code)
}

func TestFXRoutes_RateQueries(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	q := "?base=eur&quote=usd&rate_type=CLOSING&market_date=2026-06-12&set_name=GROUP&via=gbp"

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/fx/rates"+q, "").Code)
	assert.Equal(t, "get", p.x().method)
	g := p.x().gotQuery
	assert.Equal(t, "EUR", g.Base, "currency codes are upper-cased")
	assert.Equal(t, "USD", g.Quote)
	assert.Equal(t, domain.FXClosing, g.Type)
	assert.Equal(t, time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC), g.MarketDate)
	assert.Equal(t, "GROUP", g.SetName)
	assert.Equal(t, "GBP", g.Via)
	assert.True(t, g.KnownAt.IsZero())

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/fx/rates:history"+q+"&known_at=2026-06-13T08:00:00Z", "").Code)
	assert.Equal(t, "history", p.x().method)
	assert.Equal(t, time.Date(2026, 6, 13, 8, 0, 0, 0, time.UTC), p.x().gotQuery.KnownAt)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/fx/rates:explain"+q, "").Code)
	assert.Equal(t, "explain", p.x().method)

	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/fx/rates?market_date=junk", "").Code)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/fx/rates:history"+q+"&known_at=junk", "").Code)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/fx/sources/"+fxSourceID+"/rates?from=2026-06-01&to=2026-06-30&limit=5&cursor=c", "").Code)
	assert.Equal(t, fxSourceID, p.x().gotID)
	assert.Equal(t, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), p.x().gotFrom)
	assert.Equal(t, time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC), p.x().gotTo)
	assert.Equal(t, 5, p.x().gotLimit)
	assert.Equal(t, "c", p.x().gotCursor)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/fx/sources/"+fxSourceID+"/rates?from=junk", "").Code)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/fx/sources/"+fxSourceID+"/rates?limit=0", "").Code)
}

func TestFXRoutes_TypedErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{registry.ErrRateMissing, http.StatusNotFound, "FX_RATE_MISSING"},
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrSelfApproval, http.StatusForbidden, "SOD_DENIED"},
		{registry.ErrConflict, http.StatusConflict, "DUPLICATE_CANDIDATE"},
		{registry.ErrStateConflict, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrReferenceInvalid, http.StatusBadRequest, "REFERENCE_RETIRED"},
		{registry.ErrServiceUnavailable, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"},
		{registry.ErrVersionConflict, http.StatusConflict, "VERSION_CONFLICT"},
	} {
		p := &partySvc{}
		p.x().err = tc.err
		rec := do(newPartyRouter(p), http.MethodGet, "/v1/fx/rates?base=EUR&quote=USD&rate_type=SPOT&market_date=2026-06-12", "")
		assert.Equal(t, tc.status, rec.Code, tc.err.Error())
		assert.Contains(t, rec.Body.String(), tc.code, tc.err.Error())
	}
}
