package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// defineGoverned is a definition with a USD materiality threshold and, when
// permit is set, a certification profile that permits accepted risk.
func (f *fixture) defineGoverned(name string, threshold int64, permit bool) *domain.ReconciliationDefinition {
	return f.defineWith(name, func(r *domain.DefineReconciliationRequest) {
		r.Materiality = domain.MaterialityPolicy{Currencies: map[string]int64{"USD": threshold}}
		r.CertificationProfile = domain.CertificationProfile{PermitAcceptedRisk: permit}
	})
}

func (f *fixture) transition(exceptionID, actor string, req domain.TransitionExceptionRequest) (*domain.ReconciliationException, error) {
	return f.s.TransitionException(f.ctx, orgA, exceptionID, req, actor, f.claim("TransitionException", exceptionID))
}

func (f *fixture) acceptAll(runID string) {
	f.t.Helper()
	for _, e := range f.exceptions(runID) {
		if e.Status == domain.ExceptionClosed || e.Status == domain.ExceptionAcceptedRisk {
			continue
		}
		if _, err := f.transition(e.ExceptionID, "risk-approver", domain.TransitionExceptionRequest{ToState: domain.ExceptionAcceptedRisk,
			Reason: "rounding noise, below threshold", AuthorityRef: "controller-approval-7", RootCauseCode: "ROUNDING",
			RootCauseNote: "source rounds per line"}); err != nil {
			f.t.Fatalf("accept %s: %v", e.ExceptionID, err)
		}
	}
}

func byRule(t *testing.T, exs []domain.ReconciliationException) map[string][]domain.ReconciliationException {
	out := map[string][]domain.ReconciliationException{}
	for _, e := range exs {
		out[rule(t, e).Rule] = append(out[rule(t, e).Rule], e)
	}
	return out
}

// A7 / A4: every exception is raised owned, classified, severity-graded from
// its exposure against the pinned materiality, and due by the pinned SLA.
func TestGovernance_ExceptionsAreRaisedGoverned(t *testing.T) {
	f := newFixture(t)
	def := f.defineGoverned("gov-raise", 1000, false)
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100), item("REF-2", 5000),
			inCcy(item("REF-3", 7), "EUR")}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 105), inCcy(item("REF-3", 7), "EUR")}},
	})
	f.match(run.RunID)
	rules := byRule(t, f.exceptions(run.RunID))

	amount := rules[domain.RuleAmountTolerance]
	if len(amount) != 2 {
		t.Fatalf("amount exceptions: %+v", amount)
	}
	for _, e := range amount {
		if *e.Category != "MISMATCH" || *e.Assertion != "ACCURACY" || *e.ExposureMinor != 5 || *e.Severity != domain.SeverityWarning ||
			*e.Materiality != domain.Immaterial || *e.OwnerRole != "recon-owner" || e.Status != domain.ExceptionOpen {
			t.Fatalf("amount exception governance: %+v", e)
		}
		if d := e.DueAt.Sub(e.RaisedAt); d < 10*24*time.Hour-time.Minute || d > 10*24*time.Hour+time.Minute {
			t.Fatalf("WARNING due after %v, want the 10-day SLA", d)
		}
		if !containsStr(e.Attention, "UNASSIGNED") || e.AgeDays == nil {
			t.Fatalf("derived attention: %+v", e.Attention)
		}
	}
	unmatched := rules[domain.RuleUnmatched]
	if len(unmatched) != 1 || *unmatched[0].Severity != domain.SeverityBlocker || *unmatched[0].Materiality != domain.Material ||
		*unmatched[0].Category != "MISSING" || *unmatched[0].ExposureMinor != 5000 || !containsStr(unmatched[0].Attention, "MATERIAL") {
		t.Fatalf("unmatched 5000 against a 1000 threshold: %+v", unmatched)
	}
	if d := unmatched[0].DueAt.Sub(unmatched[0].RaisedAt); d > 24*time.Hour+time.Minute {
		t.Fatalf("BLOCKER due after %v, want the 1-day SLA", d)
	}
	for _, e := range rules[domain.RuleBalance] {
		if e.Severity == nil || e.Category == nil || e.ExposureMinor == nil {
			t.Fatalf("balance exception ungoverned: %+v", e)
		}
	}

	a, err := f.s.GetAssessment(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if a.ResultState != domain.ResultVariance || a.Certifiable || a.Unresolved[domain.ExceptionOpen] == 0 {
		t.Fatalf("assessment: %+v", a)
	}
}

// The worked path: assign, investigate (owner only), remediate (evidence
// required) — and a remediation still leaves the exception unresolved,
// because this run's populations are frozen (DQC-05 "Re-test").
func TestGovernance_WorkedLifecycle(t *testing.T) {
	f := newFixture(t)
	def := f.defineGoverned("gov-work", 1000, false)
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 150)}},
	})
	f.match(run.RunID)
	ex := byRule(t, f.exceptions(run.RunID))[domain.RuleAmountTolerance][0]

	if _, err := f.transition(ex.ExceptionID, "olga", domain.TransitionExceptionRequest{ToState: domain.ExceptionInvestigating, Reason: "x"}); !errors.Is(err, domain.ErrNotExceptionOwner) {
		t.Fatalf("unassigned exception worked: %v", err)
	}
	if _, err := f.s.AssignException(f.ctx, orgA, ex.ExceptionID, domain.AssignExceptionRequest{OwnerPrincipal: "olga", Reason: "AR team"},
		"lead", f.claim("AssignException", ex.ExceptionID)); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := f.transition(ex.ExceptionID, "mallory", domain.TransitionExceptionRequest{ToState: domain.ExceptionInvestigating, Reason: "x"}); !errors.Is(err, domain.ErrNotExceptionOwner) {
		t.Fatalf("non-owner worked it: %v", err)
	}
	if _, err := f.transition(ex.ExceptionID, "olga", domain.TransitionExceptionRequest{ToState: domain.ExceptionInvestigating, Reason: "checking the invoice"}); err != nil {
		t.Fatalf("investigate: %v", err)
	}
	if _, err := f.transition(ex.ExceptionID, "olga", domain.TransitionExceptionRequest{ToState: domain.ExceptionRemediated, Reason: "fixed"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("remediation without evidence: %v", err)
	}
	got, err := f.transition(ex.ExceptionID, "olga", domain.TransitionExceptionRequest{ToState: domain.ExceptionRemediated,
		Reason: "credit note issued", EvidenceRef: "ar:credit-note/CN-881"})
	if err != nil || got.Status != domain.ExceptionRemediated || *got.EvidenceRef != "ar:credit-note/CN-881" {
		t.Fatalf("remediate: %+v %v", got, err)
	}
	r, _ := f.s.GetRun(f.ctx, orgA, run.RunID)
	wantStates(t, r, domain.RunExceptionsOpen, domain.ResultVariance, domain.CertUncertified)

	hist, err := f.s.GetExceptionTransitions(f.ctx, orgA, ex.ExceptionID)
	if err != nil {
		t.Fatal(err)
	}
	var moves []string
	for _, h := range hist {
		moves = append(moves, h.FromState+">"+h.ToState+"@"+h.Actor)
	}
	if strings.Join(moves, " ") != "OPEN>OPEN@lead OPEN>INVESTIGATING@olga INVESTIGATING>REMEDIATED@olga" {
		t.Fatalf("exception history: %v", moves)
	}
}

// DQC-05 / INV-19: accepting risk needs an independent acceptor and a named
// authority, is final, and never turns the VARIANCE into a MATCH. Without a
// permitting certification profile the run cannot be certified over it.
func TestGovernance_AcceptedRiskStaysVariance(t *testing.T) {
	f := newFixture(t)
	def := f.defineGoverned("gov-accept", 1000, false)
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 103)}},
	})
	f.match(run.RunID)
	exs := f.exceptions(run.RunID)
	first := exs[0]

	if _, err := f.s.AssignException(f.ctx, orgA, first.ExceptionID, domain.AssignExceptionRequest{OwnerPrincipal: "olga", Reason: "r"},
		"lead", f.claim("AssignException", first.ExceptionID)); err != nil {
		t.Fatal(err)
	}
	accept := domain.TransitionExceptionRequest{ToState: domain.ExceptionAcceptedRisk, Reason: "immaterial", AuthorityRef: "controller-approval-7"}
	if _, err := f.transition(first.ExceptionID, "olga", accept); !errors.Is(err, domain.ErrNotIndependent) {
		t.Fatalf("owner accepted own exception: %v", err)
	}
	if _, err := f.transition(first.ExceptionID, "test-operator", accept); !errors.Is(err, domain.ErrNotIndependent) {
		t.Fatalf("raiser accepted own exception: %v", err)
	}
	f.acceptAll(run.RunID)

	got, _ := f.s.GetException(f.ctx, orgA, first.ExceptionID)
	if got.Status != domain.ExceptionAcceptedRisk || *got.AcceptedBy != "risk-approver" || got.AcceptedAt == nil || *got.AuthorityRef == "" {
		t.Fatalf("accepted exception: %+v", got)
	}
	r, _ := f.s.GetRun(f.ctx, orgA, run.RunID)
	// Nothing is being worked any more, so not ExceptionsOpen — but still a VARIANCE.
	wantStates(t, r, domain.RunRunning, domain.ResultVariance, domain.CertUncertified)
	if r.Assessment == nil || len(r.Assessment.AcceptedRiskIDs) != len(exs) || r.Assessment.Certifiable {
		t.Fatalf("the run must show its accepted risks: %+v", r.Assessment)
	}
	_, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", f.claim("Certify", run.RunID))
	if !errors.Is(err, domain.ErrRunNotEligible) || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("certify over accepted risk without a permitting profile: %v", err)
	}
	if _, err := f.transition(first.ExceptionID, "risk-approver", domain.TransitionExceptionRequest{ToState: domain.ExceptionInvestigating, Reason: "reopen"}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("accepted risk reopened: %v", err)
	}
	// Its item can no longer be manually matched away.
	var bank, ledger string
	for _, e := range exs {
		if e.Side == "Bank" {
			bank = e.ItemID
		} else if e.Side == "Ledger" {
			ledger = e.ItemID
		}
	}
	if _, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{ManualMatches: []domain.ManualMatchInput{{ItemAID: bank, ItemBID: ledger, Reason: "x"}}},
		"test-operator", f.claim("Match", run.RunID)); err == nil {
		t.Fatal("an accepted-risk item was manually matched")
	}
}

// With a profile that permits accepted risk, an immaterial accepted VARIANCE is
// certified — as a VARIANCE, with the accepted exceptions sealed into it.
func TestGovernance_CertifyOverPermittedImmaterialRisk(t *testing.T) {
	f := newFixture(t)
	def := f.defineGoverned("gov-certify", 1000, true)
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 103)}},
	})
	f.match(run.RunID)
	r, _ := f.s.GetRun(f.ctx, orgA, run.RunID)
	wantStates(t, r, domain.RunExceptionsOpen, domain.ResultVariance, domain.CertUncertified)
	f.acceptAll(run.RunID)
	r, _ = f.s.GetRun(f.ctx, orgA, run.RunID)
	wantStates(t, r, domain.RunRunning, domain.ResultVariance, domain.CertEligible)

	cert, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", f.claim("Certify", run.RunID))
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	if cert.ResultState == nil || *cert.ResultState != domain.ResultVariance || cert.Assessment == nil ||
		len(cert.Assessment.AcceptedRiskIDs) != 3 || cert.Assessment.Currencies[0].AcceptedRisk != domain.Immaterial {
		t.Fatalf("certification must record VARIANCE and every accepted risk: %+v", cert)
	}
	r, _ = f.s.GetRun(f.ctx, orgA, run.RunID)
	wantStates(t, r, domain.RunCertified, domain.ResultVariance, domain.CertCertified)
	hist := f.transitions(run.RunID)
	if !containsStr(hist, "CERTIFICATION:ELIGIBLE>CERTIFIED") || containsStr(hist, "RESULT:VARIANCE>MATCH") {
		t.Fatalf("history: %v", hist)
	}
}

// Scenario 25: individually immaterial accepted risks that are material in
// aggregate cannot be certified, even under a permitting profile.
func TestGovernance_MaterialInAggregateBlocksCertification(t *testing.T) {
	f := newFixture(t)
	def := f.defineGoverned("gov-aggregate", 1000, true)
	var bank, ledger []domain.PopulationItemInput
	for _, ref := range []string{"R1", "R2", "R3", "R4", "R5"} {
		bank = append(bank, item(ref, 1000))
		ledger = append(ledger, item(ref, 1300)) // each pair differs by 300: ERROR, immaterial
	}
	run := f.startRun(orgA, def, []testPop{{Side: "Bank", SourceSystem: "BANK", Items: bank}, {Side: "Ledger", SourceSystem: "GL", Items: ledger}})
	f.match(run.RunID)
	for _, e := range f.exceptions(run.RunID) {
		if e.Kind == domain.ExceptionKindItem && (*e.Materiality != domain.Immaterial || *e.Severity != domain.SeverityError) {
			t.Fatalf("each pair alone is immaterial: %+v", e)
		}
	}
	f.acceptAll(run.RunID)
	a, err := f.s.GetAssessment(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Certifiable || a.Currencies[0].AcceptedRisk != domain.Material || a.Currencies[0].AcceptedRiskMinor != "1500" {
		t.Fatalf("aggregate assessment: %+v", a)
	}
	if _, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", f.claim("Certify", run.RunID)); !errors.Is(err, domain.ErrRunNotEligible) ||
		!strings.Contains(err.Error(), "MATERIAL in aggregate") {
		t.Fatalf("certify material aggregate: %v", err)
	}
}

// A BLOCKER cannot be accepted (or remediated) without a root cause.
func TestGovernance_BlockerNeedsRootCause(t *testing.T) {
	f := newFixture(t)
	def := f.defineGoverned("gov-blocker", 1000, true)
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 5000)}},
		{Side: "Ledger", SourceSystem: "GL"},
	})
	f.match(run.RunID)
	ex := byRule(t, f.exceptions(run.RunID))[domain.RuleUnmatched][0]
	if _, err := f.transition(ex.ExceptionID, "risk-approver", domain.TransitionExceptionRequest{ToState: domain.ExceptionAcceptedRisk,
		Reason: "r", AuthorityRef: "a"}); !errors.Is(err, domain.ErrInvalidArgument) || !strings.Contains(err.Error(), "root_cause") {
		t.Fatalf("BLOCKER accepted without root cause: %v", err)
	}
}

// A manual match closes the exceptions of the items it reconciles through the
// governed path, recording the closure.
func TestGovernance_ManualMatchClosesWithHistory(t *testing.T) {
	f := newFixture(t)
	def := f.defineGoverned("gov-close", 1000, false)
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-X", 100)}},
	})
	f.match(run.RunID)
	var bank, ledger domain.ReconciliationException
	for _, e := range f.exceptions(run.RunID) {
		if e.Side == "Bank" {
			bank = e
		} else if e.Side == "Ledger" {
			ledger = e
		}
	}
	if _, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{ManualMatches: []domain.ManualMatchInput{
		{ItemAID: bank.ItemID, ItemBID: ledger.ItemID, Reason: "reference typo"}}}, "test-operator", f.claim("Match", run.RunID)); err != nil {
		t.Fatal(err)
	}
	got, _ := f.s.GetException(f.ctx, orgA, bank.ExceptionID)
	hist, _ := f.s.GetExceptionTransitions(f.ctx, orgA, bank.ExceptionID)
	if got.Status != domain.ExceptionClosed || got.ResolvedByMatchID == nil || len(hist) != 1 || hist[0].ToState != "CLOSED" ||
		!strings.Contains(hist[0].Reason, "reference typo") {
		t.Fatalf("closure: %+v / %+v", got, hist)
	}
}

// The database refuses what the store would refuse.
func TestGovernance_DatabaseRefusesIllegalWrites(t *testing.T) {
	f := newFixture(t)
	def := f.defineGoverned("gov-db", 1000, false)
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 103)}},
	})
	f.match(run.RunID)
	exs := f.exceptions(run.RunID)
	f.acceptAll(run.RunID)
	accepted := exs[0].ExceptionID

	open := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 103)}},
	})
	f.match(open.RunID)
	working := f.exceptions(open.RunID)[0].ExceptionID

	for name, tc := range map[string]struct{ stmt, want string }{
		"severity rewritten":   {`UPDATE reconciliation_exceptions SET severity = 'INFO' WHERE exception_id = '` + working + `'`, "may only change"},
		"due date extended":    {`UPDATE reconciliation_exceptions SET due_at = due_at + interval '30 days' WHERE exception_id = '` + working + `'`, "may only change"},
		"accepted risk undone": {`UPDATE reconciliation_exceptions SET status = 'OPEN' WHERE exception_id = '` + accepted + `'`, "immutable"},
		"closed without match": {`UPDATE reconciliation_exceptions SET status = 'CLOSED' WHERE exception_id = '` + working + `'`, "resolution_check"},
		"waived without authority": {`UPDATE reconciliation_exceptions SET status = 'ACCEPTED_RISK', accepted_by = 'x'
			WHERE exception_id = '` + working + `'`, "resolution_check"},
		"variance made eligible without a permitting profile": {`UPDATE reconciliation_runs SET certification_state = 'ELIGIBLE'
			WHERE run_id = '` + run.RunID + `'`, "state_dimensions"},
		"run governance widened": {`UPDATE reconciliation_runs SET certification_profile = '{"permit_accepted_risk": true}'
			WHERE run_id = '` + open.RunID + `'`, "pinned"},
		"exception history rewritten": {`UPDATE reconciliation_exception_transitions SET actor = 'x' WHERE exception_id = '` + accepted + `'`, "immutable"},
	} {
		tx, err := f.app.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, setErr := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA)
		_, err = tx.Exec(f.ctx, tc.stmt)
		_ = tx.Rollback(f.ctx) // before any Fatal: a leaked transaction blocks the pool's Close
		if setErr != nil {
			t.Fatal(setErr)
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v, want a refusal mentioning %q", name, err, tc.want)
		}
	}

	// Governance policies are rules: changing one is a new definition version.
	req := defineRequest("gov-db", 0)
	req.Materiality = domain.MaterialityPolicy{Currencies: map[string]int64{"USD": 1000}}
	req.CertificationProfile = domain.CertificationProfile{PermitAcceptedRisk: true}
	next, err := f.s.DefineReconciliation(f.ctx, orgA, req, "test-operator")
	if err != nil || next.Version != def.Version+1 {
		t.Fatalf("profile change: version %d (err=%v), want %d", next.Version, err, def.Version+1)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
