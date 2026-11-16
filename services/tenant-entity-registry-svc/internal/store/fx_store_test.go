package store_test

// REF-03 exchange rates against a real Postgres: ingestion with retained evidence, idempotency, outlier and manual
// rates needing independent approval, rate sets approved and certified, corrections as superseding sets, as-known-at
// history, and what the database holds whatever the service does.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	fxIngester = "p-fx-ingester"
	fxApprover = "p-fx-approver"
	fxCloser   = "p-fx-closer"
)

var fxDay = time.Now().UTC().AddDate(0, 0, -3).Format("2006-01-02")

func (f *orgFixture) fxSource(t *testing.T, code string, class domain.FXSourceClass) *domain.FXSource {
	t.Helper()
	s, err := f.s.CreateFXSource(f.ctx, domain.CreateFXSourceRequest{SourceCode: code, Name: code + " feed", SourceClass: class, Timezone: "Europe/London"}, fxIngester)
	require.NoError(t, err)
	return s
}

func (f *orgFixture) obsReq(src, base, quote string, typ domain.FXRateType, rate string, offset int) domain.IngestFXObservationRequest {
	md, _ := domain.ParseDate(fxDay)
	return domain.IngestFXObservationRequest{SourceID: src, BaseCurrency: base, QuoteCurrency: quote, RateType: typ, Rate: rate, MarketDate: fxDay,
		ObservedAt: md.Add(time.Duration(offset) * time.Minute).Add(12 * time.Hour), PayloadRef: "s3://rates/" + uuid.NewString(),
		PayloadHash: strings.Repeat("a", 64), ProviderObservationID: "prov-" + uuid.NewString()[:8]}
}

func (f *orgFixture) ingest(t *testing.T, req domain.IngestFXObservationRequest) *domain.FXObservation {
	t.Helper()
	out, err := f.s.IngestFXObservation(f.ctx, req, fxIngester)
	require.NoError(t, err)
	return &out.Observation
}

func (f *orgFixture) approveSet(t *testing.T, name string, typ domain.FXRateType, actor string, ids ...string) *domain.FXRateSet {
	t.Helper()
	set, err := f.s.ApproveFXRateSet(f.ctx, domain.ApproveFXRateSetRequest{SetName: name, RateType: typ, MarketDate: fxDay, ObservationIDs: ids, EvidenceRef: "close-pack"}, actor)
	require.NoError(t, err)
	return set
}

func (f *orgFixture) component(t *testing.T, base, quote string, typ domain.FXRateType, known time.Time) *domain.FXComponent {
	t.Helper()
	md, _ := domain.ParseDate(fxDay)
	c, err := f.s.GetApprovedFXComponent(f.ctx, base, quote, typ, md, "DEFAULT", known)
	require.NoError(t, err)
	return c
}

func TestFX_IngestRetainsTheEvidenceAndTheChecks(t *testing.T) {
	f := newORGFixture(t)
	src := f.fxSource(t, "ECB", domain.FXSourceCentral)
	req := f.obsReq(src.SourceID, "EUR", "USD", domain.FXSpot, "1.085000", 0)
	o := f.ingest(t, req)

	assert.Equal(t, domain.FXValidated, o.Status)
	assert.Equal(t, "1.085", o.Rate, "an exact decimal, canonical, never a float")
	assert.Equal(t, req.PayloadRef, o.PayloadRef, "the raw payload is kept by reference and hash")
	assert.Equal(t, req.PayloadHash, o.PayloadHash)
	assert.False(t, o.NeedsIndependent)
	var names []string
	for _, c := range o.Checks {
		names = append(names, c.Check)
	}
	assert.Equal(t, []string{"source_approved", "duplicate", "outlier"}, names)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'fx.rate.observed' AND tenant_id = $1`, f.tenantID))

	var payload string
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT payload::text FROM event_outbox WHERE event_type = 'fx.rate.observed' AND tenant_id = $1`, f.tenantID).Scan(&payload))
	assert.Contains(t, payload, `"rate": "1.085"`)
	assert.Contains(t, payload, `"rate_type": "SPOT"`)
}

func TestFX_IngestIsIdempotentAndRefusesAConflictingReplay(t *testing.T) {
	f := newORGFixture(t)
	src := f.fxSource(t, "ECB", domain.FXSourceCentral)
	req := f.obsReq(src.SourceID, "EUR", "USD", domain.FXSpot, "1.085", 0)
	first, err := f.s.IngestFXObservation(f.ctx, req, fxIngester)
	require.NoError(t, err)
	again, err := f.s.IngestFXObservation(f.ctx, req, fxIngester)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Observation.ObservationID, again.Observation.ObservationID)

	// The same provider id with another rate is a conflict, not a second rate.
	other := req
	other.Rate = "1.086"
	_, err = f.s.IngestFXObservation(f.ctx, other, fxIngester)
	require.ErrorIs(t, err, registry.ErrConflict)
	// So is the same source, pair, type and instant with no provider id.
	req2 := f.obsReq(src.SourceID, "EUR", "GBP", domain.FXSpot, "0.85", 1)
	req2.ProviderObservationID = ""
	_, err = f.s.IngestFXObservation(f.ctx, req2, fxIngester)
	require.NoError(t, err)
	req2.Rate = "0.86"
	_, err = f.s.IngestFXObservation(f.ctx, req2, fxIngester)
	require.ErrorIs(t, err, registry.ErrConflict)
	assert.Equal(t, 2, f.count(t, `SELECT count(*) FROM fx_observations WHERE tenant_id = $1`, f.tenantID))

	// Concurrent ingestions of one observation make one.
	race := f.obsReq(src.SourceID, "GBP", "JPY", domain.FXSpot, "190.5", 2)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.s.IngestFXObservation(f.ctx, race, fxIngester)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM fx_observations WHERE tenant_id = $1 AND base_currency = 'GBP'`, f.tenantID))
}

func TestFX_OutlierAndManualRatesStayObservedAndNeedIndependentApproval(t *testing.T) {
	f := newORGFixture(t)
	src := f.fxSource(t, "ECB", domain.FXSourceCentral)
	f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXSpot, "1.10", 0))

	near := f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXSpot, "1.18", 1))
	assert.Equal(t, domain.FXValidated, near.Status, "a 7% move is within the threshold")
	far := f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXSpot, "1.40", 2))
	assert.Equal(t, domain.FXObserved, far.Status, "a 27% move is an outlier: kept, flagged and not yet usable")
	assert.True(t, far.Outlier)
	assert.True(t, far.NeedsIndependent)
	var flagged bool
	for _, c := range far.Checks {
		flagged = flagged || (c.Check == "outlier" && c.Result == "FLAGGED")
	}
	assert.True(t, flagged, "the outlier evidence is retained")

	// A different type has its own history: no outlier against spot.
	avg := f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXAverage, "9.99", 3))
	assert.Equal(t, domain.FXValidated, avg.Status, "rate types are never conflated")

	man := f.fxSource(t, "MANUAL", domain.FXSourceManualExc)
	req := f.obsReq(man.SourceID, "EUR", "CHF", domain.FXClosing, "0.95", 4)
	_, err := f.s.IngestFXObservation(f.ctx, req, fxIngester)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a manual rate carries its reason and evidence")
	req.Reason, req.EvidenceRef = "provider outage at close", "treasury-memo-7"
	m := f.ingest(t, req)
	assert.Equal(t, domain.FXObserved, m.Status)
	assert.True(t, m.Manual)
	assert.True(t, m.NeedsIndependent)
}

func TestFX_SourcesAreApprovedWithdrawnAndVersioned(t *testing.T) {
	f := newORGFixture(t)
	src := f.fxSource(t, "ECB", domain.FXSourceCentral)
	_, err := f.s.CreateFXSource(f.ctx, domain.CreateFXSourceRequest{SourceCode: "ECB", Name: "dup", SourceClass: domain.FXSourceProvider}, fxIngester)
	require.ErrorIs(t, err, registry.ErrConflict)

	_, err = f.s.IngestFXObservation(f.ctx, f.obsReq(uuid.NewString(), "EUR", "USD", domain.FXSpot, "1.1", 0), fxIngester)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "only a registered source feeds rates")

	_, err = f.s.WithdrawFXSource(f.ctx, src.SourceID, domain.WithdrawFXSourceRequest{ExpectedVersion: 9, Reason: "r"}, fxApprover, false)
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.s.WithdrawFXSource(f.ctx, src.SourceID, domain.WithdrawFXSourceRequest{Reason: "r"}, fxApprover, false)
	require.ErrorIs(t, err, registry.ErrVersionRequired)
	w, err := f.s.WithdrawFXSource(f.ctx, src.SourceID, domain.WithdrawFXSourceRequest{ExpectedVersion: 1, Reason: "feed discontinued"}, fxApprover, false)
	require.NoError(t, err)
	assert.Equal(t, "WITHDRAWN", w.Status)
	_, err = f.s.IngestFXObservation(f.ctx, f.obsReq(src.SourceID, "EUR", "USD", domain.FXSpot, "1.1", 0), fxIngester)
	require.ErrorIs(t, err, registry.ErrStateConflict, "a withdrawn source takes no new rates")
	_, err = f.s.WithdrawFXSource(f.ctx, src.SourceID, domain.WithdrawFXSourceRequest{ExpectedVersion: 2, Reason: "again"}, fxApprover, false)
	require.ErrorIs(t, err, registry.ErrStateConflict)

	list, err := f.s.ListFXSources(f.ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestFX_ApprovingASetIsIndependentForExceptionalRates(t *testing.T) {
	f := newORGFixture(t)
	src := f.fxSource(t, "ECB", domain.FXSourceCentral)
	f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXClosing, "1.10", 0))
	good := f.ingest(t, f.obsReq(src.SourceID, "EUR", "GBP", domain.FXClosing, "0.85", 1))
	outlier := f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXClosing, "1.60", 2))
	require.True(t, outlier.NeedsIndependent)

	// The operator who ingested the outlying rate cannot approve it into a set.
	_, err := f.s.ApproveFXRateSet(f.ctx, domain.ApproveFXRateSetRequest{RateType: domain.FXClosing, MarketDate: fxDay,
		ObservationIDs: []string{outlier.ObservationID}, OverrideReason: "confirmed", EvidenceRef: "e"}, fxIngester)
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	// Nor without a recorded override.
	_, err = f.s.ApproveFXRateSet(f.ctx, domain.ApproveFXRateSetRequest{RateType: domain.FXClosing, MarketDate: fxDay,
		ObservationIDs: []string{outlier.ObservationID}, EvidenceRef: "e"}, fxApprover)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM fx_rate_sets WHERE tenant_id = $1`, f.tenantID))

	// Someone else, with a reason, can.
	set, err := f.s.ApproveFXRateSet(f.ctx, domain.ApproveFXRateSetRequest{RateType: domain.FXClosing, MarketDate: fxDay,
		ObservationIDs: []string{good.ObservationID, outlier.ObservationID}, OverrideReason: "confirmed with the desk", EvidenceRef: "close-pack"}, fxApprover)
	require.NoError(t, err)
	assert.Equal(t, domain.FXSetApproved, set.Status)
	assert.Equal(t, 1, set.SetVersion)
	assert.Len(t, set.Members, 2)
	for _, m := range set.Members {
		assert.Equal(t, domain.FXApproved, m.Status)
	}
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'fx.rate.approved' AND tenant_id = $1`, f.tenantID))

}
