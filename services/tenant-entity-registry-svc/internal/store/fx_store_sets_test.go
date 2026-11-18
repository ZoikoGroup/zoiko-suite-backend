package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// execTenant runs one statement inside a rolled-back transaction in the fixture's tenant, so a refusal is the
// database's own.
func (f *orgFixture) execTenant(t *testing.T, q string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, q, args...)
	return err
}

func TestFX_TheTableRefusesAnExceptionalRateApprovedByItsIngester(t *testing.T) {
	f := newORGFixture(t)
	src := f.fxSource(t, "ECB", domain.FXSourceCentral)
	f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXClosing, "1.10", 0))
	outlier := f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXClosing, "2.50", 1))
	require.True(t, outlier.NeedsIndependent)

	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID)
	require.NoError(t, err)
	setID := uuid.NewString()
	_, err = tx.Exec(ctx, `INSERT INTO fx_rate_sets (set_id, tenant_id, rate_type, market_date, evidence_ref, approved_by) VALUES ($1,$2,'CLOSING',$3,'e',$4)`,
		setID, f.tenantID, fxDay, fxIngester)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO fx_rate_set_members (tenant_id, set_id, observation_id, base_currency, quote_currency) VALUES ($1,$2,$3,'EUR','USD')`,
		f.tenantID, setID, outlier.ObservationID)
	require.Error(t, err, "a manual or outlying rate is approved by someone other than whoever ingested it, even if the service is bypassed")
	assert.Contains(t, err.Error(), "someone other than")
}

func TestFX_ASetHoldsOneRatePerPairOfItsOwnTypeAndDate(t *testing.T) {
	f := newORGFixture(t)
	src := f.fxSource(t, "ECB", domain.FXSourceCentral)
	cl := f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXClosing, "1.10", 0))
	cl2 := f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXClosing, "1.11", 1))
	spot := f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXSpot, "1.10", 2))
	other := f.obsReq(src.SourceID, "EUR", "GBP", domain.FXClosing, "0.85", 3)
	other.MarketDate = time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02")
	oldDay := f.ingest(t, other)

	approve := func(name string, typ domain.FXRateType, ids ...string) error {
		_, err := f.s.ApproveFXRateSet(f.ctx, domain.ApproveFXRateSetRequest{SetName: name, RateType: typ, MarketDate: fxDay, ObservationIDs: ids, EvidenceRef: "e"}, fxApprover)
		return err
	}
	require.ErrorIs(t, approve("A", domain.FXClosing, spot.ObservationID), registry.ErrInvalidInput, "a spot rate is not a closing rate")
	require.ErrorIs(t, approve("A", domain.FXClosing, oldDay.ObservationID), registry.ErrInvalidInput, "nor is another day's")
	require.ErrorIs(t, approve("A", domain.FXClosing, cl.ObservationID, cl2.ObservationID), registry.ErrInvalidInput, "one rate per pair")
	require.ErrorIs(t, approve("A", domain.FXClosing, uuid.NewString()), registry.ErrInvalidInput)
	require.ErrorIs(t, approve("A", domain.FXClosing, "nope"), registry.ErrInvalidInput)
	assert.Zero(t, f.count(t, `SELECT count(*) FROM fx_rate_sets WHERE tenant_id = $1`, f.tenantID), "a refused approval leaves nothing behind")

	require.NoError(t, approve("A", domain.FXClosing, cl.ObservationID))
	require.ErrorIs(t, approve("B", domain.FXClosing, cl.ObservationID), registry.ErrStateConflict, "an approved rate is in one set")
	require.ErrorIs(t, approve("A", domain.FXClosing, cl2.ObservationID), registry.ErrConflict, "one live set of a name, type and date: correct it by superseding")
	require.NoError(t, approve("B", domain.FXClosing, cl2.ObservationID), "another named set of the same type and date is its own set")
}

func TestFX_AClosedSetIsImmutableAndCorrectedByASupersedingSet(t *testing.T) {
	f := newORGFixture(t)
	src := f.fxSource(t, "ECB", domain.FXSourceCentral)
	eurusd := f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXClosing, "1.10", 0))
	eurgbp := f.ingest(t, f.obsReq(src.SourceID, "EUR", "GBP", domain.FXClosing, "0.85", 1))
	old := f.approveSet(t, "", domain.FXClosing, fxApprover, eurusd.ObservationID, eurgbp.ObservationID)

	closed, err := f.s.CloseFXRateSet(f.ctx, old.SetID, domain.CloseFXRateSetRequest{EvidenceRef: "certified"}, fxCloser)
	require.NoError(t, err)
	assert.Equal(t, domain.FXSetClosed, closed.Status)
	assert.Equal(t, fxCloser, closed.ClosedBy)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'fx.rate_set.closed' AND tenant_id = $1`, f.tenantID))
	_, err = f.s.CloseFXRateSet(f.ctx, old.SetID, domain.CloseFXRateSetRequest{EvidenceRef: "again"}, fxCloser)
	require.ErrorIs(t, err, registry.ErrStateConflict)

	// The database holds immutability whatever the service does.
	require.Error(t, f.execTenant(t, `UPDATE fx_rate_sets SET evidence_ref = 'changed' WHERE set_id = $1`, old.SetID), "a certified set is immutable")
	require.Error(t, f.execTenant(t, `UPDATE fx_rate_sets SET status = 'APPROVED' WHERE set_id = $1`, old.SetID), "and is not reopened")
	require.Error(t, f.execTenant(t, `UPDATE fx_observations SET rate = 9 WHERE observation_id = $1`, eurusd.ObservationID), "a historical rate is immutable")
	require.Error(t, f.execTenant(t, `DELETE FROM fx_observations WHERE observation_id = $1`, eurusd.ObservationID))
	require.Error(t, f.execTenant(t, `DELETE FROM fx_rate_sets WHERE set_id = $1`, old.SetID))
	require.Error(t, f.execTenant(t, `DELETE FROM fx_rate_set_members WHERE set_id = $1`, old.SetID), "membership is append-only")
	require.Error(t, f.execTenant(t, `INSERT INTO fx_rate_set_members (tenant_id, set_id, observation_id, base_currency, quote_currency) VALUES ($1,$2,$3,'EUR','GBP')`,
		f.tenantID, old.SetID, eurgbp.ObservationID), "a certified set takes no new members")
	require.Error(t, f.execTenant(t, `UPDATE fx_rate_sets SET rate_type = 'SPOT' WHERE set_id = $1`, old.SetID))

	// A rate in a set is not edited by superseding the observation: that is a set's correction.
	_, err = f.s.SupersedeFXObservation(f.ctx, eurusd.ObservationID, domain.SupersedeFXObservationRequest{Reason: "r",
		IngestFXObservationRequest: f.obsReq(src.SourceID, "EUR", "USD", domain.FXClosing, "1.12", 5)}, fxIngester)
	require.ErrorIs(t, err, registry.ErrStateConflict)

	known := time.Now().UTC()
	time.Sleep(30 * time.Millisecond)
	fixed := f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXClosing, "1.12", 6))
	corrected, err := f.s.ApproveFXRateSet(f.ctx, domain.ApproveFXRateSetRequest{RateType: domain.FXClosing, MarketDate: fxDay, SupersedesSetID: old.SetID,
		ObservationIDs: []string{fixed.ObservationID, eurgbp.ObservationID}, EvidenceRef: "correction-memo"}, fxApprover)
	require.NoError(t, err)
	assert.Equal(t, 2, corrected.SetVersion)
	assert.Equal(t, old.SetID, corrected.SupersedesSetID)

	oldNow, err := f.s.GetFXRateSet(f.ctx, old.SetID)
	require.NoError(t, err)
	assert.Equal(t, domain.FXSetSuperseded, oldNow.Status, "the old set stays, readable, as superseded")
	assert.Equal(t, corrected.SetID, oldNow.SupersededBySetID)
	byID := map[string]string{}
	for _, m := range oldNow.Members {
		byID[m.ObservationID] = m.Status
	}
	assert.Equal(t, domain.FXSuperseded, byID[eurusd.ObservationID], "the rate the correction dropped is no longer in force")
	assert.Equal(t, domain.FXApproved, byID[eurgbp.ObservationID], "the rate it carried over is")
	for name, n := range map[string]int{"fx.rate.approved": 2, "fx.rate_set.superseded": 1, "fx.rate.superseded": 1} {
		assert.Equal(t, n, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = $2 AND tenant_id = $1`, f.tenantID, name), name)
	}

	// What is in force now, and what was known before the correction.
	assert.Equal(t, "1.12", f.component(t, "EUR", "USD", domain.FXClosing, time.Time{}).Rate)
	hist := f.component(t, "EUR", "USD", domain.FXClosing, known)
	require.NotNil(t, hist)
	assert.Equal(t, "1.1", hist.Rate, "as known before the correction, the original rate: history is not silently replaced")
	assert.Equal(t, old.SetID, hist.SetID)
	assert.Equal(t, 1, hist.SetVersion)
	assert.Nil(t, f.component(t, "EUR", "USD", domain.FXClosing, known.Add(-24*time.Hour)), "before anything was approved there is no rate")
	assert.Equal(t, "0.85", f.component(t, "EUR", "GBP", domain.FXClosing, known).Rate)

	// No fallback: another type, another date and another set name have no rate.
	assert.Nil(t, f.component(t, "EUR", "USD", domain.FXSpot, time.Time{}))
	md, _ := domain.ParseDate(fxDay)
	c, err := f.s.GetApprovedFXComponent(f.ctx, "EUR", "USD", domain.FXClosing, md.AddDate(0, 0, -1), "DEFAULT", time.Time{})
	require.NoError(t, err)
	assert.Nil(t, c, "an older date is never substituted")
	c, err = f.s.GetApprovedFXComponent(f.ctx, "EUR", "USD", domain.FXClosing, md, "OTHER", time.Time{})
	require.NoError(t, err)
	assert.Nil(t, c)

	// A correction names the set it corrects, once, with the same name, type and date.
	_, err = f.s.ApproveFXRateSet(f.ctx, domain.ApproveFXRateSetRequest{RateType: domain.FXClosing, MarketDate: fxDay, SupersedesSetID: old.SetID,
		ObservationIDs: []string{fixed.ObservationID}, EvidenceRef: "e"}, fxApprover)
	require.ErrorIs(t, err, registry.ErrStateConflict, "an already superseded set is not superseded again")
	_, err = f.s.ApproveFXRateSet(f.ctx, domain.ApproveFXRateSetRequest{SetName: "OTHER", RateType: domain.FXClosing, MarketDate: fxDay, SupersedesSetID: corrected.SetID,
		ObservationIDs: []string{fixed.ObservationID}, EvidenceRef: "e"}, fxApprover)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestFX_ObservationsNotYetInASetCanBeSuperseded(t *testing.T) {
	f := newORGFixture(t)
	src := f.fxSource(t, "ECB", domain.FXSourceCentral)
	first := f.ingest(t, f.obsReq(src.SourceID, "EUR", "USD", domain.FXSpot, "1.10", 0))
	repl := f.obsReq(src.SourceID, "EUR", "USD", domain.FXSpot, "1.11", 5)
	out, err := f.s.SupersedeFXObservation(f.ctx, first.ObservationID, domain.SupersedeFXObservationRequest{Reason: "provider re-issued the print", IngestFXObservationRequest: repl}, fxIngester)
	require.NoError(t, err)
	assert.Equal(t, "1.11", out.Observation.Rate)
	require.NotNil(t, out.Superseded)
	assert.Equal(t, domain.FXSuperseded, out.Superseded.Status)
	assert.Equal(t, out.Observation.ObservationID, out.Superseded.SupersededByID)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'fx.rate.superseded' AND tenant_id = $1`, f.tenantID))

	// A superseded observation is not reused, replaced again, or approved.
	_, err = f.s.SupersedeFXObservation(f.ctx, first.ObservationID, domain.SupersedeFXObservationRequest{Reason: "r", IngestFXObservationRequest: f.obsReq(src.SourceID, "EUR", "USD", domain.FXSpot, "1.12", 6)}, fxIngester)
	require.ErrorIs(t, err, registry.ErrStateConflict)
	_, err = f.s.ApproveFXRateSet(f.ctx, domain.ApproveFXRateSetRequest{RateType: domain.FXSpot, MarketDate: fxDay, ObservationIDs: []string{first.ObservationID}, EvidenceRef: "e"}, fxApprover)
	require.ErrorIs(t, err, registry.ErrStateConflict)

	// The replacement is for the same pair, type and date, and is a different observation.
	_, err = f.s.SupersedeFXObservation(f.ctx, out.Observation.ObservationID, domain.SupersedeFXObservationRequest{Reason: "r", IngestFXObservationRequest: f.obsReq(src.SourceID, "EUR", "GBP", domain.FXSpot, "0.85", 7)}, fxIngester)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = f.s.SupersedeFXObservation(f.ctx, out.Observation.ObservationID, domain.SupersedeFXObservationRequest{Reason: "r", IngestFXObservationRequest: repl}, fxIngester)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "re-submitting the same observation replaces nothing")
	_, err = f.s.SupersedeFXObservation(f.ctx, uuid.NewString(), domain.SupersedeFXObservationRequest{Reason: "r", IngestFXObservationRequest: repl}, fxIngester)
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestFX_ListBySourceTenantIsolationAndRLS(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	src := a.fxSource(t, "ECB", domain.FXSourceCentral)
	for i, rate := range []string{"1.10", "1.11", "1.12"} {
		req := a.obsReq(src.SourceID, "EUR", "USD", domain.FXSpot, rate, i)
		req.MarketDate = time.Now().UTC().AddDate(0, 0, -i-1).Format("2006-01-02")
		a.ingest(t, req)
	}
	all, next, err := a.s.ListFXRatesBySource(a.ctx, src.SourceID, time.Time{}, time.Time{}, 0, "")
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Empty(t, next)
	assert.True(t, all[0].MarketDate > all[1].MarketDate, "newest market date first")

	p1, next, err := a.s.ListFXRatesBySource(a.ctx, src.SourceID, time.Time{}, time.Time{}, 2, "")
	require.NoError(t, err)
	require.Len(t, p1, 2)
	require.NotEmpty(t, next)
	p2, next2, err := a.s.ListFXRatesBySource(a.ctx, src.SourceID, time.Time{}, time.Time{}, 2, next)
	require.NoError(t, err)
	assert.Len(t, p2, 1)
	assert.Empty(t, next2)

	from := time.Now().UTC().AddDate(0, 0, -2)
	ranged, _, err := a.s.ListFXRatesBySource(a.ctx, src.SourceID, from, time.Time{}, 0, "")
	require.NoError(t, err)
	assert.Len(t, ranged, 2)
	_, _, err = a.s.ListFXRatesBySource(a.ctx, src.SourceID, time.Time{}, time.Time{}, 0, "junk")
	require.ErrorIs(t, err, registry.ErrInvalidInput)

	_, _, err = b.s.ListFXRatesBySource(b.ctx, src.SourceID, time.Time{}, time.Time{}, 0, "")
	require.ErrorIs(t, err, registry.ErrNotFound)
	srcs, err := b.s.ListFXSources(b.ctx)
	require.NoError(t, err)
	assert.Empty(t, srcs)
	_, err = b.s.IngestFXObservation(b.ctx, b.obsReq(src.SourceID, "EUR", "USD", domain.FXSpot, "1.1", 0), fxIngester)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "another tenant's source is no source here")
	set, err := b.s.GetFXRateSet(b.ctx, uuid.NewString())
	require.NoError(t, err)
	assert.Nil(t, set)
}

// Migration 000022 reverses cleanly while no rate exists, applies again, and refuses with one.
func TestFX_MigrationDownAndUp(t *testing.T) {
	f := newORGFixture(t)
	ctx := context.Background()
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}
	exists := func(q string) bool {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx, q).Scan(&n))
		return n > 0
	}
	const table = `SELECT count(*) FROM pg_tables WHERE tablename = 'fx_observations'`

	f.fxSource(t, "ECB", domain.FXSourceCentral)
	_, err := f.pool.Exec(ctx, read("000022_fx_rates.down.sql"))
	require.Error(t, err, "down is refused while sources or observations exist")
	assert.Contains(t, err.Error(), "000022 down refused")

	f2 := newORGFixture(t)
	_, err = f2.pool.Exec(ctx, read("000022_fx_rates.down.sql"))
	require.NoError(t, err)
	assert.False(t, exists(table))
	_, err = f2.pool.Exec(ctx, read("000022_fx_rates.up.sql"))
	require.NoError(t, err, "up applies again after down")
	assert.True(t, exists(table))
}
