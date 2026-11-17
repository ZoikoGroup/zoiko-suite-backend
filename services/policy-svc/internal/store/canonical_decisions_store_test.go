package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/policy-svc/internal/domain"
)

const storeMatrix = `{"currency":"GBP","valuation_basis":"GROSS","instrument_types":["SUPPLIER_CONTRACT"],` +
	`"bands":[{"band_id":"B1","above":null,"up_to":"10000","min_distinct_signers":1,"required_offices":[{"office":"FINANCE_SIGNER","min_count":1}]},` +
	`{"band_id":"B2","above":"10000","up_to":null,"min_distinct_signers":2,"required_offices":[{"office":"DIRECTOR","min_count":1}],"eligible_offices":["DIRECTOR","CFO"]}]}`

func signedEnvelope(id string) json.RawMessage {
	return json.RawMessage(`{"decision_id":"` + id + `","outcome":"NOT_CONFIGURED","integrity":{"alg":"EdDSA","kid":"k","value":"sig"}}`)
}

// ZS-GOV-INT-002: a decision no rule produced is evidence too, with its
// signed envelope, reasons and validity; only non-permissive results may
// lack policy provenance, and an unsigned envelope is refused.
func TestCanonicalDecisions_NotConfiguredIsEvidence(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := uuid.NewString()
	until := e.clock.Add(15 * time.Minute)
	ev := &domain.Evaluation{
		EvaluationID: id, TenantID: tenantA, DecisionID: "req-nc", LegalEntityID: sp(entityA), PolicyType: "SIGNATORY_MATRIX",
		RequestedByPrincipalID: "user", InputFingerprint: "sha256:f", ActionContext: []byte(`{"instrument_type":"GUARANTEE"}`),
		AsOf: e.clock, KnownAt: e.clock, Result: "NOT_CONFIGURED", Outcome: "NOT_CONFIGURED",
		RuleBasis: "SIGNATORY_MATRIX:unresolved", SelectedRuleIDs: []string{"SIGNATORY_MATRIX#not_configured"},
		EvaluatorVersion: "policy-svc/SIGNATORY_MATRIX@1", CorrelationID: "corr", EvaluatedAt: e.clock,
		ContractVersion: "1.0", DecisionOutcome: "NOT_CONFIGURED", ReasonCodes: []string{"CONFIG_INSTRUMENT_MATRIX_MISSING"},
		ValidUntil: &until, DecisionEnvelope: signedEnvelope(id),
	}
	saved, created, err := e.s.SaveEvaluation(ctx, ev)
	if err != nil || !created {
		t.Fatalf("save: created=%v err=%v", created, err)
	}
	if saved.EvaluationID != id || saved.PolicyVersionID != "" || saved.DecisionOutcome != "NOT_CONFIGURED" ||
		len(saved.ReasonCodes) != 1 || saved.ValidUntil == nil || !saved.ValidUntil.Equal(until) || !saved.EvaluatedAt.Equal(e.clock) {
		t.Fatalf("read back %+v", saved)
	}
	// Byte for byte: a replay serves exactly what was signed and answered.
	if string(saved.DecisionEnvelope) != string(signedEnvelope(id)) {
		t.Fatalf("envelope changed in storage: %s", saved.DecisionEnvelope)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM policy_evidence_outbox WHERE evaluation_id = $1`, id); n != 1 {
		t.Fatal("a NOT_CONFIGURED decision is relayed to the ledger like any other")
	}
	got, err := e.s.FindEvaluationByDecision(ctx, tenantA, "req-nc")
	if err != nil || got.EvaluationID != id {
		t.Fatalf("by request id: %+v %v", got, err)
	}

	// A permissive or holding result always names its version.
	for _, result := range []string{"PERMIT", "SIGNATURE_REQUIRED", "WITHIN_THRESHOLD"} {
		bad := *ev
		bad.EvaluationID, bad.DecisionID, bad.Result, bad.DecisionOutcome = uuid.NewString(), "req-"+result, result, ""
		bad.DecisionEnvelope = nil
		if _, _, err := e.s.SaveEvaluation(ctx, &bad); err == nil {
			t.Fatalf("%s without policy provenance was stored", result)
		}
	}
	unsigned := *ev
	unsigned.EvaluationID, unsigned.DecisionID = uuid.NewString(), "req-unsigned"
	unsigned.DecisionEnvelope = json.RawMessage(`{"outcome":"NOT_CONFIGURED"}`)
	if _, _, err := e.s.SaveEvaluation(ctx, &unsigned); err == nil {
		t.Fatal("an unsigned envelope was stored")
	}
	odd := *ev
	odd.EvaluationID, odd.DecisionID, odd.DecisionOutcome = uuid.NewString(), "req-odd", "ALLOW"
	odd.DecisionEnvelope = signedEnvelope(odd.EvaluationID)
	if _, _, err := e.s.SaveEvaluation(ctx, &odd); err == nil {
		t.Fatal("an outcome outside the contract vocabulary was stored")
	}
}

// Activating a SIGNATORY_MATRIX version publishes the Doc 02 §10.2 event in
// the same transaction; the status card reflects it.
func TestSignatoryMatrix_PublishedEventAndStatusCard(t *testing.T) {
	e := newEnv(t)
	ctx := as(tenantA)
	p, created, err := e.s.CreatePolicy(ctx, domain.CreatePolicyParams{
		PolicyCode: "SIG-UK", PolicyName: "UK signatory matrix", PolicyType: "SIGNATORY_MATRIX", TenantID: sp(tenantA), CreatedByPrincipalID: "author",
	})
	if err != nil || !created {
		t.Fatalf("family: %v", err)
	}
	other := e.family(t, tenantA, sp(tenantA), "AP-1")
	e.release(t, tenantA, e.draft(t, tenantA, other, sp(tenantA), nil, e.clock.Add(-time.Hour), `{"threshold_amount": 5}`))

	v := e.release(t, tenantA, e.draft(t, tenantA, p, sp(tenantA), sp(entityA), e.clock.Add(-time.Hour), storeMatrix))
	if v.VersionStatus != domain.StatusActive {
		t.Fatalf("status %s", v.VersionStatus)
	}
	var payload []byte
	if err := e.admin.QueryRow(context.Background(),
		`SELECT payload FROM outbox WHERE event_type = 'policy.signatory_matrix.published' AND aggregate_id = $1`, p.PolicyID).Scan(&payload); err != nil {
		t.Fatalf("published event: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(payload, &body)
	if body["policy_version_id"] != v.PolicyVersionID || body["currency"] != "GBP" || body["policy_code"] != "SIG-UK" {
		t.Fatalf("payload %s", payload)
	}
	if _, leaked := body["bands"]; leaked {
		t.Fatal("the event must not carry the matrix content")
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE event_type = 'policy.signatory_matrix.published'`); n != 1 {
		t.Fatalf("only the signatory family publishes it, got %d", n)
	}

	saved, _, err := e.s.SaveEvaluation(context.Background(), &domain.Evaluation{
		TenantID: tenantA, DecisionID: "req-1", LegalEntityID: sp(entityA), PolicyType: "SIGNATORY_MATRIX", InputFingerprint: "sha256:f",
		ActionContext: []byte(`{}`), AsOf: e.clock, KnownAt: e.clock, Result: "SIGNATURE_REQUIRED", Outcome: "SIGNATURE_REQUIRED",
		PolicyID: p.PolicyID, PolicyVersionID: v.PolicyVersionID, PolicyCode: "SIG-UK", ArtifactDigest: v.ArtifactDigest,
		RuleBasis: "SIG-UK:" + v.PolicyVersionID, SelectedRuleIDs: []string{"x"}, EvaluatorVersion: "t", CorrelationID: "c",
		DecisionOutcome: "SIGNATURE_REQUIRED", ContractVersion: "1.0",
	})
	if err != nil || saved.PolicyVersionID != v.PolicyVersionID {
		t.Fatalf("save: %v", err)
	}

	cards, err := e.s.FamilyStatuses(as(tenantA))
	if err != nil {
		t.Fatal(err)
	}
	c := cards["SIGNATORY_MATRIX"]
	if c == nil || c.Families != 1 || c.VersionsByStatus["ACTIVE"] != 1 || len(c.GoverningVersions) != 1 || c.Decisions24h["SIGNATURE_REQUIRED"] != 1 {
		t.Fatalf("card %+v", c)
	}
	g := c.GoverningVersions[0]
	if g.PolicyVersionID != v.PolicyVersionID || g.ScopeType != domain.ScopeTypeLegalEntity || g.ApprovedByPrincipalID == nil ||
		*g.ApprovedByPrincipalID != "approver" || g.ActivatedByPrincipalID == nil || *g.ActivatedByPrincipalID != "activator" {
		t.Fatalf("governing %+v", g)
	}
	if cards["APPROVAL_THRESHOLD"] == nil || cards["APPROVAL_THRESHOLD"].VersionsByStatus["ACTIVE"] != 1 {
		t.Fatalf("threshold card %+v", cards["APPROVAL_THRESHOLD"])
	}
	// Another tenant sees none of it.
	if cards, _ := e.s.FamilyStatuses(as(tenantB)); cards["SIGNATORY_MATRIX"] != nil {
		t.Fatalf("tenant B sees tenant A's family: %+v", cards["SIGNATORY_MATRIX"])
	}
}

// 000013 rolls back and forward on a database without provenance-less rows.
func TestMigration000013_DownUp(t *testing.T) {
	dsn := adminDSN(t)
	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	resetSchema(t, admin)
	applyMigrations(t, admin, "", "")
	for _, f := range []string{"000013_canonical_decisions.down.sql", "000013_canonical_decisions.up.sql"} {
		sql, err := readMigration(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

func readMigration(name string) (string, error) {
	b, err := os.ReadFile(filepath.Join("..", "..", "deployments", "migrations", name))
	return string(b), err
}

// SIGNATORY_MATRIX is evaluable, so PDC-05 applies: no activation without a
// certified suite, and its cases run through the same evaluator as live
// resolution (SIGNATURE_REQUIRED, PERMIT, NOT_CONFIGURED are results).
func TestSignatoryMatrix_CertificationGateApplies(t *testing.T) {
	e := newEnv(t)
	gs := e.gated()
	p, _, err := e.s.CreatePolicy(as(tenantA), domain.CreatePolicyParams{
		PolicyCode: "SIG-CERT", PolicyName: "matrix", PolicyType: "SIGNATORY_MATRIX", TenantID: sp(tenantA), CreatedByPrincipalID: "author",
	})
	if err != nil {
		t.Fatal(err)
	}
	v := e.reviewedAndApproved(t, e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(-time.Hour), storeMatrix))
	if err := activate(gs, v); !errors.Is(err, domain.ErrCertificationRequired) {
		t.Fatalf("an uncertified matrix must not go live, got %v", err)
	}
	for code, c := range map[string]struct{ ctx, expected string }{
		"band-2":        {`{"instrument_type":"SUPPLIER_CONTRACT","amount":"10000.01","currency":"GBP","basis":"GROSS"}`, "SIGNATURE_REQUIRED"},
		"usd":           {`{"instrument_type":"SUPPLIER_CONTRACT","amount":"1","currency":"USD","basis":"GROSS"}`, "NOT_CONFIGURED"},
		"high-risk":     {`{"instrument_type":"GUARANTEE","amount":"1","currency":"GBP","basis":"GROSS"}`, "NOT_CONFIGURED"},
		"float-refused": {`{"instrument_type":"SUPPLIER_CONTRACT","amount":"1e3","currency":"GBP","basis":"GROSS"}`, "INPUT_ERROR"},
	} {
		if _, _, err := e.s.CreateTestCase(as(tenantA), domain.CreateTestCaseParams{
			PolicyID: p.PolicyID, TestCode: code, Layer: domain.TestLayerBoundary, Mandatory: true,
			Description: code, ActionContext: json.RawMessage(c.ctx), ExpectedResult: c.expected, ActorID: "tester",
		}); err != nil {
			t.Fatalf("case %s: %v", code, err)
		}
	}
	run, err := e.s.RunTests(as(tenantA), v.PolicyVersionID, "tester", domain.EventMeta{})
	if err != nil || run.RunStatus != domain.TestRunPass || run.EvaluatorVersion != "policy-svc/SIGNATORY_MATRIX@1" {
		t.Fatalf("run %+v err %v", run, err)
	}
	if err := activate(gs, v); err != nil {
		t.Fatalf("a certified matrix must go live: %v", err)
	}
}

// Doc 01 §5.4: an instrument-specific high-risk matrix sits beside the
// ordinary one at the same scope; two matrices that would govern the same
// instrument in the same regime are refused at activation (§3.2).
func TestSignatoryMatrix_CoexistenceAndOverlapGuard(t *testing.T) {
	e := newEnv(t)
	matrixFamily := func(code string) *domain.Policy {
		p, _, err := e.s.CreatePolicy(as(tenantA), domain.CreatePolicyParams{
			PolicyCode: code, PolicyName: code, PolicyType: "SIGNATORY_MATRIX", TenantID: sp(tenantA), CreatedByPrincipalID: "author",
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	start := e.clock.Add(-time.Hour)
	ordinary := e.release(t, tenantA, e.draft(t, tenantA, matrixFamily("SIG-ORD"), sp(tenantA), sp(entityA), start, storeMatrix))
	if ordinary.VersionStatus != domain.StatusActive {
		t.Fatal(ordinary.VersionStatus)
	}
	highRisk := `{"currency":"GBP","valuation_basis":"GROSS","high_risk":true,"instrument_types":["GUARANTEE","SUPPLIER_CONTRACT"],` +
		`"bands":[{"band_id":"H","above":null,"up_to":null,"min_distinct_signers":2,"required_offices":[{"office":"DIRECTOR","min_count":2}]}]}`
	hr := e.release(t, tenantA, e.draft(t, tenantA, matrixFamily("SIG-HR"), sp(tenantA), sp(entityA), start, highRisk))
	if hr.VersionStatus != domain.StatusActive {
		t.Fatalf("a high-risk matrix must coexist with the ordinary one: %s", hr.VersionStatus)
	}
	disjoint := `{"currency":"GBP","valuation_basis":"GROSS","instrument_types":["EMPLOYMENT_AGREEMENT"],` +
		`"bands":[{"band_id":"A","above":null,"up_to":null,"min_distinct_signers":1,"required_offices":[{"office":"HR_DIRECTOR","min_count":1}]}]}`
	if v := e.release(t, tenantA, e.draft(t, tenantA, matrixFamily("SIG-HR-EMP"), sp(tenantA), sp(entityA), start, disjoint)); v.VersionStatus != domain.StatusActive {
		t.Fatal("a matrix for other instruments must coexist")
	}
	dup := e.draft(t, tenantA, matrixFamily("SIG-DUP"), sp(tenantA), sp(entityA), start, storeMatrix)
	for _, step := range []struct{ cmd, actor string }{{domain.CommandSubmitReview, "author"}, {domain.CommandApprove, "approver"}} {
		res, err := e.cmd(t, tenantA, dup, step.cmd, step.actor)
		if err != nil {
			t.Fatal(err)
		}
		dup = res.Version
	}
	if _, err := e.cmd(t, tenantA, dup, domain.CommandActivate, "activator"); !errors.Is(err, domain.ErrPolicyConflicted) {
		t.Fatalf("a second matrix for SUPPLIER_CONTRACT at the same scope must be refused, got %v", err)
	}
}
