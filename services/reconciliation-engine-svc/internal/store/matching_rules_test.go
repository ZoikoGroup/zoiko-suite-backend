package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

func (f *fixture) defineWith(name string, mutate func(*domain.DefineReconciliationRequest)) *domain.ReconciliationDefinition {
	f.t.Helper()
	req := defineRequest(name, 0)
	mutate(&req)
	return f.defineApproved(orgA, req)
}

func (f *fixture) match(runID string) *domain.ReconciliationRun {
	f.t.Helper()
	r, err := f.s.Match(f.ctx, orgA, runID, domain.MatchRequest{}, "test-operator", f.claim("Match", runID))
	if err != nil {
		f.t.Fatalf("match: %v", err)
	}
	return r
}

func (f *fixture) exceptions(runID string) []domain.ReconciliationException {
	f.t.Helper()
	ex, err := f.s.GetExceptions(f.ctx, orgA, runID)
	if err != nil {
		f.t.Fatalf("get exceptions: %v", err)
	}
	return ex
}

func rule(t *testing.T, e domain.ReconciliationException) domain.ExceptionDetail {
	t.Helper()
	var d domain.ExceptionDetail
	if err := json.Unmarshal(e.Detail, &d); err != nil {
		t.Fatalf("exception %s has no readable detail: %v", e.ExceptionID, err)
	}
	return d
}

func withDims(it domain.PopulationItemInput, kv ...string) domain.PopulationItemInput {
	it.Dimensions = map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		it.Dimensions[kv[i]] = kv[i+1]
	}
	return it
}

func inCcy(it domain.PopulationItemInput, ccy string) domain.PopulationItemInput {
	it.Currency = ccy
	return it
}

// A3: the definition's declared composite key decides pairing.
func TestMatching_CompositeKeyIsHonoured(t *testing.T) {
	f := newFixture(t)
	def := f.defineWith("composite", func(r *domain.DefineReconciliationRequest) {
		r.MatchKeyFields = []string{"ref_id", "dimensions.account_code"}
	})
	if len(def.MatchKeyFields) != 2 {
		t.Fatalf("definition keys: %v", def.MatchKeyFields)
	}
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{
			withDims(item("R1", 100), "account_code", "1100"), withDims(item("R1", 200), "account_code", "1200")}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{
			withDims(item("R1", 100), "account_code", "1200"), withDims(item("R1", 200), "account_code", "1100")}},
	})
	if run.MatchKeyFields[1] != "dimensions.account_code" {
		t.Fatalf("run does not pin the match key: %v", run.MatchKeyFields)
	}
	if got := f.match(run.RunID); got.Status != domain.RunExceptionsOpen {
		t.Fatalf("on ref_id alone these pair by amount; on the declared key nothing reconciles: %s", got.Status)
	}
	for _, e := range f.exceptions(run.RunID) {
		if e.Kind == domain.ExceptionKindItem && rule(t, e).Rule != domain.RuleAmountTolerance {
			t.Fatalf("item exception %+v, want AMOUNT_OUTSIDE_TOLERANCE (same key, different amount)", e)
		}
	}
}

// DQC-03 / NP-21: every item matches and the aggregate balances, but one
// entity's sub-total does not — the run cannot certify.
func TestMatching_SubDimensionCannotHideInAggregate(t *testing.T) {
	f := newFixture(t)
	def := f.defineWith("dimensional", func(r *domain.DefineReconciliationRequest) {
		r.BalanceDimensions = []string{"entity"}
	})
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{
			withDims(item("R1", 100), "entity", "E1"), withDims(item("R2", 100), "entity", "E2")}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{
			withDims(item("R1", 100), "entity", "E1"), withDims(item("R2", 100), "entity", "E1")}},
	})
	if got := f.match(run.RunID); got.Status != domain.RunExceptionsOpen {
		t.Fatalf("status %s, want ExceptionsOpen", got.Status)
	}
	results, _ := f.s.GetMatchResults(f.ctx, orgA, run.RunID)
	ex := f.exceptions(run.RunID)
	if len(results) != 2 || len(ex) != 2 {
		t.Fatalf("want both items matched and two balance exceptions; got %d matches, %+v", len(results), ex)
	}
	for _, e := range ex {
		d := rule(t, e)
		if e.Kind != domain.ExceptionKindBalance || e.Dimension == nil || *e.Dimension != "entity" || e.ItemID != "" ||
			d.Rule != domain.RuleBalance || d.SideTotals == nil {
			t.Fatalf("balance exception: %+v / %+v", e, d)
		}
	}
	if _, err := f.s.Certify(f.ctx, orgA, run.RunID, "test-operator", f.claim("Certify", run.RunID)); !errors.Is(err, domain.ErrOpenExceptionsRemain) {
		t.Fatalf("certify with an unbalanced sub-dimension: %v", err)
	}
	// Re-running the matcher does not raise the same variance twice.
	if _, err := f.s.Reperform(f.ctx, orgA, run.RunID, "test-operator", f.claim("Reperform", run.RunID)); err != nil {
		t.Fatalf("reperform: %v", err)
	}
	if n := len(f.exceptions(run.RunID)); n != 2 {
		t.Fatalf("balance exceptions after reperform: %d, want 2", n)
	}
}

// A2: same reference, same amount, different currency is a currency mismatch
// — never a match — and the per-currency totals do not offset each other.
func TestMatching_CurrencyMismatch(t *testing.T) {
	f := newFixture(t)
	def := f.defineWith("currencies", func(r *domain.DefineReconciliationRequest) {
		big := int64(1 << 40)
		r.Tolerance.Currencies["EUR"] = domain.CurrencyTolerance{AbsoluteMinorUnits: &big}
	})
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{inCcy(item("R1", 500), "EUR")}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("R1", 500)}},
	})
	f.match(run.RunID)
	var items, balances int
	for _, e := range f.exceptions(run.RunID) {
		switch e.Kind {
		case domain.ExceptionKindItem:
			items++
			if d := rule(t, e); d.Rule != domain.RuleCurrencyMismatch || d.CounterpartItemID == "" {
				t.Fatalf("item exception: %+v", d)
			}
		case domain.ExceptionKindBalance:
			balances++
			if e.Dimension == nil || *e.Dimension != "" {
				t.Fatalf("population-level balance exception expected: %+v", e)
			}
		}
	}
	// The USD total (0 vs 500) is out of its zero tolerance; the EUR total
	// (500 vs 0) is within the generous EUR tolerance declared above.
	if items != 2 || balances != 1 {
		t.Fatalf("want 2 currency-mismatch item exceptions and one unbalanced USD total; got %d, %d", items, balances)
	}

	// A manual match cannot pair them either.
	ex := f.exceptions(run.RunID)
	var a, b string
	for _, e := range ex {
		if e.Kind == domain.ExceptionKindItem && e.Side == "Bank" {
			a = e.ItemID
		} else if e.Kind == domain.ExceptionKindItem {
			b = e.ItemID
		}
	}
	_, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{ManualMatches: []domain.ManualMatchInput{
		{ItemAID: a, ItemBID: b, Reason: "same payment"}}}, "reviewer", f.claim("Match", "manual"))
	if !errors.Is(err, domain.ErrManualMatchOutsideTolerance) {
		t.Fatalf("manual match across currencies: %v", err)
	}
}

// A5: percentage tolerance of an explicit base side, and date tolerance.
func TestMatching_PercentageAndDateTolerance(t *testing.T) {
	f := newFixture(t)
	def := f.defineWith("pct-date", func(r *domain.DefineReconciliationRequest) {
		bps := int64(100) // 1% of the Ledger amount
		days := 2
		r.Tolerance = domain.TolerancePolicy{Currencies: map[string]domain.CurrencyTolerance{"USD": {PercentageBps: &bps}},
			PercentageBaseSide: "Ledger", DateDays: &days}
	})
	late := item("R3", 1000)
	late.OccurredAt = late.OccurredAt.Add(3 * 24 * time.Hour)
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("R1", 1009), item("R2", 1011), item("R3", 1000)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("R1", 1000), item("R2", 1000), late}},
	})
	if run.Tolerance == nil || run.Tolerance.PercentageBaseSide != "Ledger" || *run.Tolerance.DateDays != 2 {
		t.Fatalf("run does not pin the tolerance policy: %+v", run.Tolerance)
	}
	f.match(run.RunID)
	got := map[string]string{}
	for _, e := range f.exceptions(run.RunID) {
		if e.Kind == domain.ExceptionKindItem {
			got[e.RefID] = rule(t, e).Rule
		}
	}
	if _, matched := got["R1"]; matched {
		t.Fatalf("R1 differs by 9 of an allowed 10 (1%% of 1000): %v", got)
	}
	if got["R2"] != domain.RuleAmountTolerance || got["R3"] != domain.RuleDateTolerance {
		t.Fatalf("exceptions by ref: %v", got)
	}
}

// A2: no cross-currency total is written anywhere; certification carries
// per-side, per-currency totals.
func TestCertification_PerCurrencyTotalsOnly(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "per-currency-cert", 0)
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("R1", 100), inCcy(item("R2", 7), "EUR")}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("R1", 100), inCcy(item("R2", 7), "EUR")}},
	})
	snaps, _ := f.s.GetPopulationSnapshots(f.ctx, orgA, run.RunID)
	for _, s := range snaps {
		if s.TotalAmountMinorUnits != nil || len(s.ControlTotals) != 2 {
			t.Fatalf("snapshot totals: %+v", s)
		}
	}
	f.match(run.RunID)
	cert, err := f.s.Certify(f.ctx, orgA, run.RunID, "test-operator", f.claim("Certify", run.RunID))
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	if cert.TotalAssertedMinor != nil || len(cert.ControlTotals) != 2 || len(cert.ControlTotals["Bank"]) != 2 {
		t.Fatalf("certification totals: %+v", cert)
	}
}

// Tolerance is pinned from the moment the run starts, not only once certified.
func TestRun_ToleranceCannotBeWidenedBeforeCertification(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "pinned-open", 0)
	run := f.startRun(orgA, def, cleanPops())
	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE reconciliation_runs SET tolerance_policy = '{"currencies":{"USD":{"absolute_minor_units":999999}}}' WHERE run_id = $1`,
		`UPDATE reconciliation_runs SET match_key_fields = '{currency}' WHERE run_id = $1`,
	} {
		if _, err := tx.Exec(f.ctx, "SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(f.ctx, stmt, run.RunID)
		if err == nil || !strings.Contains(err.Error(), "pinned scope or rules") {
			t.Fatalf("%s on an open run: %v", stmt, err)
		}
		if _, err := tx.Exec(f.ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDefinition_RulesValidatedAndVersioned(t *testing.T) {
	f := newFixture(t)
	bad := defineRequest("bad-key", 0)
	bad.MatchKeyFields = []string{"reference"}
	if _, err := f.s.DefineReconciliation(f.ctx, orgA, bad, "test-operator", f.claim("Define", "bad")); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("unknown match key accepted: %v", err)
	}
	first := f.defineReconciliation(orgA, "rules-v", 0)
	withDim := f.publish(first, func(r *domain.RulesInput) { r.BalanceDimensions = []string{"entity"} })
	if withDim.Version != first.Version+1 || len(withDim.BalanceDimensions) != 1 {
		t.Fatalf("balance dimension change: version %d, want %d", withDim.Version, first.Version+1)
	}
	days := 1
	withDate := f.publish(withDim, func(r *domain.RulesInput) { r.Tolerance.DateDays = &days })
	if withDate.Version != withDim.Version+1 || withDate.Tolerance.DateDays == nil || withDate.RulesSHA256 == withDim.RulesSHA256 {
		t.Fatalf("tolerance policy change: version %d, want %d", withDate.Version, withDim.Version+1)
	}
}
