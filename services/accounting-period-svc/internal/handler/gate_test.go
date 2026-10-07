package handler_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/service"
)

func resolved(t *testing.T, e *testEnv, entity, date string, kv ...string) service.Resolution {
	t.Helper()
	rec := e.resolve(entity, date, kv...)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	return decode[service.Resolution](t, rec)
}

// The gate NEVER defaults to open: no period for the entity/date => 404
// PERIOD_NOT_FOUND with posting_allowed=false.
func TestGate_NoPeriodMeansNotAllowedNeverOpen(t *testing.T) {
	e := newEnv(t)

	// Before anything was materialised at all.
	rec := e.resolve(entityA, "2026-03-10")
	b := e.expectErr(rec, http.StatusNotFound, domain.CodePeriodNotFound)
	require.NotNil(t, b.PostingAllowed)
	assert.False(t, *b.PostingAllowed)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	e.materialize("v1", nil)
	assert.True(t, resolved(t, e, entityA, "2026-03-10").PostingAllowed)

	// A date outside every period, another entity, and a scope nobody materialised.
	e.expectErr(e.resolve(entityA, "2027-01-01"), http.StatusNotFound, domain.CodePeriodNotFound)
	e.expectErr(e.resolve(entityA, "2025-12-31"), http.StatusNotFound, domain.CodePeriodNotFound)
	e.expectErr(e.resolve("entity-b", "2026-03-10"), http.StatusNotFound, domain.CodePeriodNotFound)
	e.materialize("v1", map[string]any{"book_scope": "BOOK-1", "reason": "book 1"})
	assert.True(t, resolved(t, e, entityA, "2026-03-10", "book_scope", "BOOK-1").PostingAllowed)
	e.expectErr(e.resolve(entityA, "2027-03-10", "book_scope", "BOOK-1"), http.StatusNotFound, domain.CodePeriodNotFound)
}

func TestGate_InputValidation(t *testing.T) {
	e := newEnv(t)
	e.materialize("v1", nil)
	for name, path := range map[string]string{
		"no entity":       "/v1/accounting-periods:resolve?date=2026-03-10",
		"no date":         "/v1/accounting-periods:resolve?legal_entity_id=entity-a",
		"bad date":        "/v1/accounting-periods:resolve?legal_entity_id=entity-a&date=10/03/2026",
		"bad purpose":     "/v1/accounting-periods:resolve?legal_entity_id=entity-a&date=2026-03-10&purpose=delete",
		"bad kind":        "/v1/accounting-periods:resolve?legal_entity_id=entity-a&date=2026-03-10&kind=X",
		"impossible date": "/v1/accounting-periods:resolve?legal_entity_id=entity-a&date=2026-02-30",
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.do(http.MethodGet, path, nil)
			b := e.expectErr(rec, http.StatusUnprocessableEntity, domain.CodeContextInvalid)
			require.NotNil(t, b.PostingAllowed)
			assert.False(t, *b.PostingAllowed)
		})
	}
	rec := e.do(http.MethodGet, "/v1/accounting-periods:resolve?legal_entity_id=entity-a&date=2026-03-10", nil, opts{noTenant: true})
	require.Equal(t, http.StatusUnauthorized, rec.Code, "no tenant, no answer")
}

func TestGate_PostingRulePerState(t *testing.T) {
	e := newEnv(t)
	res := e.materialize("v1", nil)
	mk := func(key string, st domain.State) string {
		id := e.periodByKey(res, key).PeriodID
		e.drive(id, st)
		return id
	}
	mk("FY2026-P01", domain.StateOpen)
	mk("FY2026-P02", domain.StateSoftClosed)
	mk("FY2026-P03", domain.StateHardClosed)
	mk("FY2026-P04", domain.StateReopenAuthorized)
	mk("FY2026-P05", domain.StateReclosed)

	cases := []struct {
		date, extra string
		state       domain.State
		allowed     bool
		mode        string
		reason      string
	}{
		{"2026-01-15", "", domain.StateOpen, true, "ALLOWED", domain.ReasonOpen},
		{"2026-02-15", "", domain.StateSoftClosed, false, "RESTRICTED", domain.ReasonSoftClosed},
		{"2026-02-15", "soft_close_exception", domain.StateSoftClosed, true, "RESTRICTED", domain.ReasonSoftClosedException},
		{"2026-03-15", "", domain.StateHardClosed, false, "BLOCKED", domain.ReasonHardClosed},
		{"2026-03-15", "soft_close_exception", domain.StateHardClosed, false, "BLOCKED", domain.ReasonHardClosed},
		{"2026-04-15", "", domain.StateReopenAuthorized, true, "RESTRICTED", domain.ReasonReopenInScope},
		{"2026-05-15", "", domain.StateReclosed, false, "BLOCKED", domain.ReasonReclosed},
		{"2026-05-15", "soft_close_exception", domain.StateReclosed, false, "BLOCKED", domain.ReasonReclosed},
	}
	for _, c := range cases {
		t.Run(c.date+"/"+string(c.state)+"/"+c.extra, func(t *testing.T) {
			kv := []string{}
			if c.extra != "" {
				kv = []string{"soft_close_exception", "true"}
			}
			r := resolved(t, e, entityA, c.date, kv...)
			assert.Equal(t, c.state, r.State)
			assert.Equal(t, c.allowed, r.PostingAllowed)
			assert.Equal(t, c.mode, r.PostingMode)
			assert.Equal(t, c.reason, r.Reason)
			assert.NotEmpty(t, r.PeriodID)
			assert.Equal(t, r.Version, r.StateVersion)
			assert.Greater(t, r.Version, int64(0))
		})
	}
	// Period boundaries are inclusive on both ends.
	assert.Equal(t, "FY2026-P03", resolved(t, e, entityA, "2026-03-01").PeriodKey)
	assert.Equal(t, "FY2026-P03", resolved(t, e, entityA, "2026-03-31").PeriodKey)
	assert.Equal(t, "FY2026-P04", resolved(t, e, entityA, "2026-04-01").PeriodKey)
}

func TestGate_ReopenOnlyInsideScopeAndBeforeExpiry(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P03").PeriodID
	e.drive(id, domain.StateHardClosed)
	e.must(e.cmd(id, "authorize-reopen", approver, func(b map[string]any) {
		b["reopen_scope"] = map[string]any{"book_scope": "BOOK-1", "module_scope": "AP"}
		b["expires_at"] = e.now.Add(time.Hour).Format(time.RFC3339)
	}))
	date := "2026-03-10"

	in := resolved(t, e, entityA, date, "book_scope", "BOOK-1", "module_scope", "AP")
	assert.True(t, in.PostingAllowed)
	assert.Equal(t, domain.ReasonReopenInScope, in.Reason)

	for name, kv := range map[string][]string{
		"other book":          {"book_scope", "BOOK-2", "module_scope", "AP"},
		"other module":        {"book_scope", "BOOK-1", "module_scope", "AR"},
		"no module presented": {"book_scope", "BOOK-1"},
		"no scope presented":  {},
	} {
		r := resolved(t, e, entityA, date, kv...)
		assert.False(t, r.PostingAllowed, name)
		assert.Equal(t, domain.ReasonReopenOutOfScope, r.Reason, name)
		assert.Equal(t, domain.StateReopenAuthorized, r.State)
	}

	// The window is judged at read time against the clock: no sweeper involved.
	e.now = e.now.Add(time.Hour).Add(-time.Nanosecond)
	assert.True(t, resolved(t, e, entityA, date, "book_scope", "BOOK-1", "module_scope", "AP").PostingAllowed, "one tick before expires_at")
	e.now = e.now.Add(time.Nanosecond)
	r := resolved(t, e, entityA, date, "book_scope", "BOOK-1", "module_scope", "AP")
	assert.False(t, r.PostingAllowed, "at expires_at the window is closed")
	assert.Equal(t, domain.ReasonReopenExpired, r.Reason)
	assert.Equal(t, domain.StateReopenAuthorized, r.State, "stored state is unchanged; only the gate answer expired")

	// An expired window can still be reclosed (that is how it is normally ended).
	e.must(e.cmd(id, "reclose", approver))
	assert.Equal(t, domain.ReasonReclosed, resolved(t, e, entityA, date).Reason)
}

func TestGate_AmbiguityAcrossCalendarVersionsIsRuleAmbiguous(t *testing.T) {
	e := newEnv(t)
	e.cal.previews["v2"] = quarterly("v2")
	v1 := e.materialize("v1", nil)
	e.materialize("v2", nil)

	// Both versions are OPEN on the same date: same outcome, deterministic answer.
	a := resolved(t, e, entityA, "2026-03-10")
	b := resolved(t, e, entityA, "2026-03-10")
	assert.True(t, a.PostingAllowed)
	assert.Equal(t, a.PeriodID, b.PeriodID, "ties break deterministically")

	// Close the v1 March period: the v2 quarter covering it is still OPEN.
	e.drive(e.periodByKey(v1, "FY2026-P03").PeriodID, domain.StateHardClosed)
	rec := e.resolve(entityA, "2026-03-10")
	body := e.expectErr(rec, http.StatusConflict, domain.CodeRuleAmbiguous)
	require.NotNil(t, body.PostingAllowed)
	assert.False(t, *body.PostingAllowed)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

}

func TestGate_SpecialPeriodNeedsKindToDisambiguate(t *testing.T) {
	e := newEnv(t)
	pv := e.cal.previews["v1"]
	pv.Periods = append(append([]service.CalendarPeriod(nil), pv.Periods...),
		service.CalendarPeriod{PeriodKey: "FY2026-ADJ", PeriodNo: 13, StartDate: "2026-12-15", EndDate: "2026-12-31", Kind: "SPECIAL"})
	e.cal.previews["v1"] = pv
	res := e.materialize("v1", nil)
	e.drive(e.periodByKey(res, "FY2026-P12").PeriodID, domain.StateHardClosed)

	e.expectErr(e.resolve(entityA, "2026-12-20"), http.StatusConflict, domain.CodeRuleAmbiguous)
	assert.False(t, resolved(t, e, entityA, "2026-12-20", "kind", "NORMAL").PostingAllowed)
	adj := resolved(t, e, entityA, "2026-12-20", "kind", "SPECIAL")
	assert.True(t, adj.PostingAllowed)
	assert.Equal(t, "FY2026-ADJ", adj.PeriodKey)
}

func TestGate_EntityWideAndBookSpecificPeriodsWithSameOutcome(t *testing.T) {
	e := newEnv(t)
	e.materialize("v1", nil)
	e.materialize("v1", map[string]any{"book_scope": "BOOK-1", "reason": "book"})
	r := resolved(t, e, entityA, "2026-03-10", "book_scope", "BOOK-1")
	assert.True(t, r.PostingAllowed)
	assert.Len(t, e.store.Periods(), 24)
	got := e.get(r.PeriodID)
	assert.Equal(t, "BOOK-1", got.BookScope, "the most specific scope wins when outcomes agree")

	// Without a book the entity-wide period answers; a book-specific hard close does not leak into it.
	e.drive(got.PeriodID, domain.StateHardClosed)
	assert.True(t, resolved(t, e, entityA, "2026-03-10").PostingAllowed)
	// And for BOOK-1 the entity-wide OPEN and the book HARD_CLOSED conflict: ambiguity, not "most specific wins".
	e.expectErr(e.resolve(entityA, "2026-03-10", "book_scope", "BOOK-1"), http.StatusConflict, domain.CodeRuleAmbiguous)
}

func TestGate_ReadsThePrimaryNoCachedState(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P03").PeriodID
	assert.True(t, resolved(t, e, entityA, "2026-03-10").PostingAllowed)
	e.drive(id, domain.StateHardClosed)
	r := resolved(t, e, entityA, "2026-03-10")
	assert.False(t, r.PostingAllowed, "a commit is visible to the very next gate read")
}

func TestStatusByKey_CompatMappingAndUnknownIs404(t *testing.T) {
	e := newEnv(t)
	res := e.materialize("v1", nil)
	states := map[string]domain.State{
		"FY2026-P01": domain.StateOpen, "FY2026-P02": domain.StateSoftClosed, "FY2026-P03": domain.StateHardClosed,
		"FY2026-P04": domain.StateReopenAuthorized, "FY2026-P05": domain.StateReclosed,
	}
	want := map[string]string{"FY2026-P01": "OPEN", "FY2026-P02": "CLOSED", "FY2026-P03": "LOCKED", "FY2026-P04": "OPEN", "FY2026-P05": "CLOSED"}
	for key, st := range states {
		e.drive(e.periodByKey(res, key).PeriodID, st)
	}
	for key, w := range want {
		rec := e.do(http.MethodGet, "/v1/accounting-periods:status-by-key?legal_entity_id="+entityA+"&period_key="+key, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		assert.Equal(t, w, decode[map[string]string](t, rec)["close_status"], key)
	}
	// The legacy parameter name is accepted.
	rec := e.do(http.MethodGet, "/v1/accounting-periods:status-by-key?legal_entity_id="+entityA+"&period_name=FY2026-P03", nil)
	assert.Equal(t, "LOCKED", decode[map[string]string](t, rec)["close_status"])

	// Unknown period: 404, NEVER OPEN (this is the fail-open being eliminated).
	rec = e.do(http.MethodGet, "/v1/accounting-periods:status-by-key?legal_entity_id="+entityA+"&period_key=FY2099-P01", nil)
	e.expectErr(rec, http.StatusNotFound, domain.CodePeriodNotFound)
	rec = e.do(http.MethodGet, "/v1/accounting-periods:status-by-key?legal_entity_id=entity-b&period_key=FY2026-P01", nil)
	e.expectErr(rec, http.StatusNotFound, domain.CodePeriodNotFound)
	rec = e.do(http.MethodGet, "/v1/accounting-periods:status-by-key?legal_entity_id="+entityA, nil)
	e.expectErr(rec, http.StatusUnprocessableEntity, domain.CodeContextInvalid)

	// An expired reopen window reads CLOSED, consistent with the gate.
	e.now = e.now.Add(2 * time.Hour)
	rec = e.do(http.MethodGet, "/v1/accounting-periods:status-by-key?legal_entity_id="+entityA+"&period_key=FY2026-P04", nil)
	assert.Equal(t, "CLOSED", decode[map[string]string](t, rec)["close_status"])
}

func TestStatusByKey_SeveralPeriodsWithTheKeyReturnTheMostRestrictive(t *testing.T) {
	e := newEnv(t)
	e.materialize("v1", nil)
	b1 := e.materialize("v1", map[string]any{"book_scope": "BOOK-1", "reason": "b"})
	e.drive(e.periodByKey(b1, "FY2026-P03").PeriodID, domain.StateHardClosed)
	rec := e.do(http.MethodGet, "/v1/accounting-periods:status-by-key?legal_entity_id="+entityA+"&period_key=FY2026-P03", nil)
	assert.Equal(t, "LOCKED", decode[map[string]string](t, rec)["close_status"])
}
