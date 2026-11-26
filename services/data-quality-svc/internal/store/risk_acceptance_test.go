package store_test

import (
	"errors"
	"testing"

	"zoiko.io/data-quality-svc/internal/domain"
)

// ZS-SVC-Z-001 DQC-05 section 8 (docs/architecture/DQC_GAP_MATRIX.md, step H): certification over a failed mandatory
// rule needs a GOVERNED risk acceptance - requested by one person, approved by two others, certified by a fourth.

const (
	requester = "risk-requester"
	approver1 = "risk-approver-1"
	approver2 = "risk-approver-2"
	approver3 = "risk-approver-3"
	certifier = "risk-certifier"
)

func riskable(allow bool) domain.ContractBody {
	b := profiled(domain.CompatBackward)
	b.CertificationProfile.AllowRiskAcceptance = allow
	return b
}

// failedBlocker publishes a contract and records a run whose BLOCKER rule failed. The run is ExceptionsOpen.
func (f *fixture) failedBlocker(key string, allow bool) (*domain.ContractVersion, *domain.DQRun, domain.DQResult) {
	f.t.Helper()
	c := f.published(orgA, key, riskable(allow))
	run := f.mustContractRun(c, "pop-"+key, fail("amount-positive"), pass("invoice-unique"), pass("memo-present"), pass("format-ok"))
	if run.Status != domain.RunExceptionsOpen {
		f.t.Fatalf("setup: run is %s", run.Status)
	}
	return c, run, resultFor(f.t, f, run.RunID, "amount-positive")
}

func (f *fixture) askRisk(runID, resultID, by string) (*domain.RiskAcceptance, error) {
	return f.s.RequestRiskAcceptance(f.ctx, orgA, runID, by, domain.RequestRiskAcceptanceRequest{
		ResultID: resultID, Reason: "the AR ledger is being restated; the amount rule is known to misfire on credit notes", EvidenceRefs: []string{"incident-901"},
	}, f.claim("RequestRiskAcceptance", resultID+by))
}

func (f *fixture) approveRisk(id, by string) (*domain.RiskAcceptance, error) {
	return f.s.ApproveRiskAcceptance(f.ctx, orgA, id, by, domain.ApproveRiskAcceptanceRequest{Reason: "reviewed the evidence"}, f.claim("ApproveRiskAcceptance", id+by))
}

func (f *fixture) certifyBy(run *domain.DQRun, by string) (*domain.DQCertification, error) {
	return f.s.CertifyDQ(f.ctx, orgA, run.RunID, by, domain.CertifyDQRequest{}, f.claim("CertifyDQ", run.RunID+by))
}

// accepted drives a risk acceptance to ACCEPTED.
func (f *fixture) accepted(runID, resultID string) *domain.RiskAcceptance {
	f.t.Helper()
	k, err := f.askRisk(runID, resultID, requester)
	if err != nil {
		f.t.Fatalf("request: %v", err)
	}
	if _, err := f.approveRisk(k.AcceptanceID, approver1); err != nil {
		f.t.Fatalf("approve 1: %v", err)
	}
	k, err = f.approveRisk(k.AcceptanceID, approver2)
	if err != nil || k.Status != domain.RiskAccepted {
		f.t.Fatalf("approve 2: %+v %v", k, err)
	}
	return k
}

func TestRisk_FullGovernedFlowCertifiesOverAFailedBlocker(t *testing.T) {
	f := newContractFixture(t)
	c, run, res := f.failedBlocker("risk.flow", true)

	// An exception is raised on the failed result, as it would be in practice; accepting its risk resolves it.
	if _, err := f.s.RaiseIssue(f.ctx, orgA, domain.RaiseIssueRequest{RunID: run.RunID, ResultID: res.ResultID, Description: "amount rule failed"}, "op", f.claim("RaiseIssue", "risk-flow")); err != nil {
		t.Fatal(err)
	}
	// Before any acceptance the run is uncertifiable, exactly as it always was.
	if _, err := f.certifyBy(run, certifier); !errors.Is(err, domain.ErrRunHasOpenIssues) {
		t.Fatalf("certified a failed run with no acceptance: %v", err)
	}

	k, err := f.askRisk(run.RunID, res.ResultID, requester)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if k.Status != domain.RiskProposed || len(k.Approvals) != 0 || k.ApprovalsRequired != 2 || k.RuleKey != "amount-positive" || k.Severity == nil || *k.Severity != domain.SeverityBlocker {
		t.Fatalf("a new request: %+v", k)
	}
	if _, err := f.certifyBy(run, certifier); err == nil {
		t.Fatal("certified over a risk nobody has approved")
	}
	k, err = f.approveRisk(k.AcceptanceID, approver1)
	if err != nil || k.Status != domain.RiskProposed || len(k.Approvals) != 1 {
		t.Fatalf("one approval: %+v %v", k, err)
	}
	if _, err := f.certifyBy(run, certifier); err == nil {
		t.Fatal("certified over a risk with one of two approvals")
	}
	k, err = f.approveRisk(k.AcceptanceID, approver2)
	if err != nil || k.Status != domain.RiskAccepted || len(k.Approvals) != 2 {
		t.Fatalf("two approvals: %+v %v", k, err)
	}

	cert, err := f.certifyBy(run, certifier)
	if err != nil {
		t.Fatalf("certify over an accepted risk: %v", err)
	}
	// The certification says on its face what it accepted, and who approved.
	if len(cert.Summary.AcceptedRisks) != 1 {
		t.Fatalf("accepted risks on the certification: %+v", cert.Summary.AcceptedRisks)
	}
	a := cert.Summary.AcceptedRisks[0]
	if a.AcceptanceID != k.AcceptanceID || a.RuleKey != "amount-positive" || a.RequestedBy != requester || len(a.Approvers) != 2 || a.Approvers[0] != approver1 || a.Approvers[1] != approver2 {
		t.Fatalf("accepted risk: %+v", a)
	}
	if got, _ := f.s.GetRun(f.ctx, orgA, run.RunID); got.Status != domain.RunCertified {
		t.Fatalf("run after certification: %s", got.Status)
	}
	failing := 0
	for _, r := range cert.Summary.Results {
		if r.Status == domain.ResultFail {
			failing++
		}
	}
	if failing != 1 || cert.Summary.ContractID != c.ContractID {
		t.Fatalf("the failure was hidden by the certification: %d failing results", failing)
	}
	count := func(event string) int {
		var n int
		_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, k.AcceptanceID, event).Scan(&n)
		return n
	}
	if count("dqc.risk.requested") != 1 || count("dqc.risk.approved") != 2 || count("dqc.risk.accepted") != 1 {
		t.Fatalf("events: requested=%d approved=%d accepted=%d", count("dqc.risk.requested"), count("dqc.risk.approved"), count("dqc.risk.accepted"))
	}
}

// The certifier must be none of the people who asked for or approved the acceptance.
func TestRisk_CertifierMustBeIndependent(t *testing.T) {
	f := newContractFixture(t)
	_, run, res := f.failedBlocker("risk.indep", true)
	f.accepted(run.RunID, res.ResultID)
	for _, who := range []string{requester, approver1, approver2} {
		if _, err := f.certifyBy(run, who); !errors.Is(err, domain.ErrRiskCertifierNotIndependent) {
			t.Fatalf("%s certified over their own acceptance: %v", who, err)
		}
	}
	if _, err := f.certifyBy(run, certifier); err != nil {
		t.Fatalf("an independent certifier was refused: %v", err)
	}
}

func TestRisk_OnlyAFailedMandatoryResultOfAnOpenRunCanBeAccepted(t *testing.T) {
	f := newContractFixture(t)

	// A contract that does not permit it.
	_, run, res := f.failedBlocker("risk.denied", false)
	if _, err := f.askRisk(run.RunID, res.ResultID, requester); !errors.Is(err, domain.ErrRiskNotPermitted) {
		t.Fatalf("a contract that does not allow risk acceptance: %v", err)
	}

	// Missing evidence is not a risk: accepting it would turn an absence into a pass.
	c := f.published(orgA, "risk.indeterminate", riskable(true))
	missing := f.mustContractRun(c, "pop-risk-missing", pass("amount-positive"), pass("memo-present"), pass("format-ok")) // invoice-unique unreported
	ind := resultFor(t, f, missing.RunID, "invoice-unique")
	if ind.Status != domain.ResultIndeterminate {
		t.Fatalf("setup: %s", ind.Status)
	}
	if _, err := f.askRisk(missing.RunID, ind.ResultID, requester); !errors.Is(err, domain.ErrRiskNotAcceptable) {
		t.Fatalf("an Indeterminate result: %v", err)
	}

	// A failing rule the profile does not require needs nothing accepted.
	c2 := f.published(orgA, "risk.optional", riskable(true))
	opt := f.mustContractRun(c2, "pop-risk-opt", fail("amount-positive"), pass("invoice-unique"), fail("memo-present"), pass("format-ok"))
	memo := resultFor(t, f, opt.RunID, "memo-present")
	if _, err := f.askRisk(opt.RunID, memo.ResultID, requester); !errors.Is(err, domain.ErrRiskNotAcceptable) {
		t.Fatalf("a non-mandatory result: %v", err)
	}

	// A passing result is not a risk, a result of another run is not this run's, a rule-set run has no profile.
	c3, run3, res3 := f.failedBlocker("risk.shape", true)
	pas := resultFor(t, f, run3.RunID, "invoice-unique")
	if _, err := f.askRisk(run3.RunID, pas.ResultID, requester); !errors.Is(err, domain.ErrRiskNotAcceptable) {
		t.Fatalf("a Pass: %v", err)
	}
	if _, err := f.askRisk(run3.RunID, ind.ResultID, requester); !errors.Is(err, domain.ErrResultNotFound) {
		t.Fatalf("another run's result: %v", err)
	}
	v := f.publishRuleSet(orgA, "risk-legacy", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	lr, err := f.run(v, "pop-risk-legacy", domain.RuleOutcome{RuleKey: "r-a", PassCount: 90, FailCount: 10, FailureMagnitudeSum: "5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.askRisk(lr.RunID, resultFor(t, f, lr.RunID, "r-a").ResultID, requester); !errors.Is(err, domain.ErrRiskNotPermitted) {
		t.Fatalf("a rule-set run: %v", err)
	}
	_ = c3

	// A run that is no longer open.
	clean := f.mustContractRun(c3, "pop-risk-clean", allPass()...)
	if _, err := f.askRisk(clean.RunID, resultFor(t, f, clean.RunID, "amount-positive").ResultID, requester); !errors.Is(err, domain.ErrRiskNotAcceptable) {
		t.Fatalf("a clean run: %v", err)
	}

	// Proof that those refusals were about their own defect: the failed mandatory result is accepted for request,
	// and a second request for the same result is refused as a duplicate.
	if _, err := f.askRisk(run3.RunID, res3.ResultID, requester); err != nil {
		t.Fatalf("a valid request was refused: %v", err)
	}
	if _, err := f.askRisk(run3.RunID, res3.ResultID, approver1); !errors.Is(err, domain.ErrRiskAlreadyRequested) {
		t.Fatalf("a second request for one result: %v", err)
	}
}

func TestRisk_RequestNeedsAReasonAndEvidence(t *testing.T) {
	f := newContractFixture(t)
	_, run, res := f.failedBlocker("risk.shape2", true)
	good := domain.RequestRiskAcceptanceRequest{ResultID: res.ResultID, Reason: "known misfire", EvidenceRefs: []string{"e1"}}
	for name, mutate := range map[string]func(*domain.RequestRiskAcceptanceRequest){
		"no result":   func(r *domain.RequestRiskAcceptanceRequest) { r.ResultID = "" },
		"no reason":   func(r *domain.RequestRiskAcceptanceRequest) { r.Reason = "  " },
		"no evidence": func(r *domain.RequestRiskAcceptanceRequest) { r.EvidenceRefs = nil },
		"blank ref":   func(r *domain.RequestRiskAcceptanceRequest) { r.EvidenceRefs = []string{" "} },
	} {
		r := good
		mutate(&r)
		if _, err := f.s.RequestRiskAcceptance(f.ctx, orgA, run.RunID, requester, r, f.claim("RequestRiskAcceptance", name)); !errors.Is(err, domain.ErrRiskInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := f.s.RequestRiskAcceptance(f.ctx, orgA, run.RunID, requester, good, f.claim("RequestRiskAcceptance", "good")); err != nil {
		t.Fatalf("a good request was refused: %v", err)
	}
	approve := domain.ApproveRiskAcceptanceRequest{Reason: " "}
	list, _ := f.s.ListRiskAcceptances(f.ctx, orgA, run.RunID)
	if _, err := f.s.ApproveRiskAcceptance(f.ctx, orgA, list[0].AcceptanceID, approver1, approve, f.claim("ApproveRiskAcceptance", "blank")); !errors.Is(err, domain.ErrRiskInvalid) {
		t.Fatalf("an approval with no reason: %v", err)
	}
}

func TestRisk_ApprovalRules(t *testing.T) {
	f := newContractFixture(t)
	_, run, res := f.failedBlocker("risk.approve", true)
	k, err := f.askRisk(run.RunID, res.ResultID, requester)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.approveRisk(k.AcceptanceID, requester); !errors.Is(err, domain.ErrRiskApproverIsRequester) {
		t.Fatalf("the requester approved: %v", err)
	}
	claim := f.claim("ApproveRiskAcceptance", "keyed")
	if _, err := f.s.ApproveRiskAcceptance(f.ctx, orgA, k.AcceptanceID, requester, domain.ApproveRiskAcceptanceRequest{Reason: "x"}, claim); !errors.Is(err, domain.ErrRiskApproverIsRequester) {
		t.Fatal(err)
	}
	var keys int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM idempotency_keys WHERE idempotency_key = $1`, claim.Key).Scan(&keys)
	if keys != 0 {
		t.Fatalf("a refused approval consumed its key: %d", keys)
	}
	if _, err := f.approveRisk(k.AcceptanceID, approver1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.approveRisk(k.AcceptanceID, approver1); !errors.Is(err, domain.ErrRiskAlreadyApproved) {
		t.Fatalf("one person approving twice: %v", err)
	}
	if _, err := f.approveRisk(k.AcceptanceID, approver2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.approveRisk(k.AcceptanceID, approver3); !errors.Is(err, domain.ErrRiskAlreadyAccepted) {
		t.Fatalf("a third approval of an accepted risk: %v", err)
	}
	if _, err := f.approveRisk("dqk_00000000-0000-0000-0000-000000000000", approver1); !errors.Is(err, domain.ErrRiskNotFound) {
		t.Fatalf("an unknown acceptance: %v", err)
	}
	// Another tenant cannot see or approve it.
	if _, err := f.s.ApproveRiskAcceptance(f.ctx, orgB, k.AcceptanceID, approver3, domain.ApproveRiskAcceptanceRequest{Reason: "x"}, f.claim("ApproveRiskAcceptance", "b")); !errors.Is(err, domain.ErrRiskNotFound) {
		t.Fatalf("tenant B approved tenant A's acceptance: %v", err)
	}
}

// Every mandatory result that is not a Pass needs its own accepted risk.
func TestRisk_EveryFailedMandatoryRuleNeedsItsOwn(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "risk.two", riskable(true))
	run := f.mustContractRun(c, "pop-risk-two", fail("amount-positive"), fail("invoice-unique"), pass("memo-present"), pass("format-ok"))
	blocker, errRule := resultFor(t, f, run.RunID, "amount-positive"), resultFor(t, f, run.RunID, "invoice-unique")
	f.accepted(run.RunID, blocker.ResultID)
	if _, err := f.certifyBy(run, certifier); !errors.Is(err, domain.ErrRunHasOpenIssues) {
		t.Fatalf("certified with one of two failed rules accepted: %v", err)
	}
	k2, err := f.askRisk(run.RunID, errRule.ResultID, requester)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.approveRisk(k2.AcceptanceID, approver1)
	if _, err := f.certifyBy(run, certifier); err == nil {
		t.Fatal("certified with the second risk half approved")
	}
	if _, err := f.approveRisk(k2.AcceptanceID, approver2); err != nil {
		t.Fatal(err)
	}
	cert, err := f.certifyBy(run, certifier)
	if err != nil || len(cert.Summary.AcceptedRisks) != 2 {
		t.Fatalf("both accepted: %+v %v", cert, err)
	}
}

// An accepted risk never covers missing evidence: a mandatory rule with no result still blocks.
func TestRisk_AcceptedRiskDoesNotCoverMissingEvidence(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "risk.mixed", riskable(true))
	run := f.mustContractRun(c, "pop-risk-mixed", fail("amount-positive"), pass("memo-present"), pass("format-ok")) // invoice-unique unreported
	f.accepted(run.RunID, resultFor(t, f, run.RunID, "amount-positive").ResultID)
	if _, err := f.certifyBy(run, certifier); !errors.Is(err, domain.ErrRunHasOpenIssues) {
		t.Fatalf("certified over missing mandatory evidence: %v", err)
	}
}

// The certification of accepted risk is a different basis from any clean one, and a revocation still works.
func TestRisk_CertificationRestsOnTheAcceptanceAndCanBeRevoked(t *testing.T) {
	f := newContractFixture(t)
	_, run, res := f.failedBlocker("risk.revoke", true)
	f.accepted(run.RunID, res.ResultID)
	cert, err := f.certifyBy(run, certifier)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.revoke(run.RunID, "control-owner", falsePass); err != nil {
		t.Fatalf("revoking a certification that rested on an accepted risk: %v", err)
	}
	got, _ := f.s.GetCertification(f.ctx, orgA, run.RunID)
	if got.Status != domain.CertificationRevoked || len(got.Summary.AcceptedRisks) != 1 || got.SummarySHA256 != cert.SummarySHA256 {
		t.Fatalf("after revocation: %+v", got)
	}
}

// The same rules, held by the database, for a statement that skips the service.
func TestRisk_DatabaseHoldsTheRules(t *testing.T) {
	f := newContractFixture(t)
	certify := func(runID string) error {
		_, err := f.admin.Exec(f.ctx, `UPDATE dq_runs SET status = 'Certified', completed_at = now() WHERE run_id = $1`, runID)
		return err
	}

	// A failed run, no acceptance: the database refuses to certify it.
	_, run, res := f.failedBlocker("risk.db", true)
	if certify(run.RunID) == nil {
		t.Fatal("the database let a failed run be certified with no acceptance")
	}
	// A contract that does not permit it.
	_, denied, _ := f.failedBlocker("risk.db.denied", false)
	if certify(denied.RunID) == nil {
		t.Fatal("the database let a run be certified over a failed rule under a contract that forbids it")
	}
	// A rule-set run.
	v := f.publishRuleSet(orgA, "risk-db-legacy", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	lr, err := f.run(v, "pop-risk-db-legacy", domain.RuleOutcome{RuleKey: "r-a", PassCount: 90, FailCount: 10, FailureMagnitudeSum: "5"})
	if err != nil {
		t.Fatal(err)
	}
	if certify(lr.RunID) == nil {
		t.Fatal("the database let a failed rule-set run be certified")
	}
	// One approval is not enough.
	k, err := f.askRisk(run.RunID, res.ResultID, requester)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.approveRisk(k.AcceptanceID, approver1); err != nil {
		t.Fatal(err)
	}
	if certify(run.RunID) == nil {
		t.Fatal("the database accepted one approval where two are required")
	}

	// The requester cannot approve their own request, even by SQL.
	ins := `INSERT INTO dq_risk_acceptance_approvals (tenant_id, acceptance_id, approver, reason) VALUES ($1, $2, $3, 'r')`
	if _, err := f.admin.Exec(f.ctx, ins, orgA, k.AcceptanceID, requester); err == nil {
		t.Fatal("the database let the requester approve their own request")
	}
	// An approver cannot approve twice.
	if _, err := f.admin.Exec(f.ctx, ins, orgA, k.AcceptanceID, approver1); err == nil {
		t.Fatal("the database let one person approve twice")
	}
	// Records are append-only.
	for name, stmt := range map[string]string{
		"change an acceptance": `UPDATE dq_risk_acceptances SET reason = 'x' WHERE acceptance_id = $1`,
		"delete an acceptance": `DELETE FROM dq_risk_acceptances WHERE acceptance_id = $1`,
		"delete an approval":   `DELETE FROM dq_risk_acceptance_approvals WHERE acceptance_id = $1`,
		"change an approval":   `UPDATE dq_risk_acceptance_approvals SET approver = 'x' WHERE acceptance_id = $1`,
	} {
		if _, err := f.admin.Exec(f.ctx, stmt, k.AcceptanceID); err == nil {
			t.Fatalf("the database allowed: %s", name)
		}
	}
	// An acceptance on an Indeterminate result is refused by the database too.
	c := f.published(orgA, "risk.db.ind", riskable(true))
	missing := f.mustContractRun(c, "pop-risk-db-missing", pass("amount-positive"), pass("memo-present"), pass("format-ok"))
	ind := resultFor(t, f, missing.RunID, "invoice-unique")
	insK := `INSERT INTO dq_risk_acceptances (acceptance_id, tenant_id, run_id, result_id, reason, evidence_refs, requested_by) VALUES ($1, $2, $3, $4, 'r', ARRAY['e'], 'x')`
	if _, err := f.admin.Exec(f.ctx, insK, "dqk_"+newTestRunID()[4:], orgA, missing.RunID, ind.ResultID); err == nil {
		t.Fatal("the database accepted a risk on an Indeterminate result")
	}

	// And with two approvals the same statement is accepted: it was the approvals that were missing.
	if _, err := f.approveRisk(k.AcceptanceID, approver2); err != nil {
		t.Fatal(err)
	}
	if err := certify(run.RunID); err != nil {
		t.Fatalf("the database refused a certification with a fully accepted risk: %v", err)
	}
}
