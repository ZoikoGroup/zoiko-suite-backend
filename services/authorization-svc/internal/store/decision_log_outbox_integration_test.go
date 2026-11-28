package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/identity-envelope/decisionledger"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/ledger"
	"zoiko.io/authorization-svc/internal/store"
)

// DL-3 (25 Nov 2026, D-DL-1) on a real database (AUTHZ_IT_DSN, migrated to
// 000047, as the NOBYPASSRLS runtime role): a DENY or REQUIRE_APPROVAL writes
// its governance-ledger record into decision_log_outbox in the decision's own
// transaction; a PERMIT or STEP_UP writes none; the row is visible to its
// tenant and the relay only.

// ledgerRows counts decision_log_outbox rows for decisionID as tenant sees them.
func ledgerRows(t *testing.T, pool *pgxpool.Pool, tenant, decisionID string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM decision_log_outbox WHERE decision_id = $1`, decisionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDL3_OnlyDenyAndRequireApprovalAreQueuedForTheLedger(t *testing.T) {
	pool := outboxPool(t)
	s := store.New(pool, zap.NewNop())
	s.SetDecisionLedger(ledger.Builder("svc-authorization", ""))
	tenant := uuid.NewString()

	record := func(decision, outcome string) string {
		t.Helper()
		d, err := s.RecordAccessDecision(context.Background(), domain.RecordAccessDecisionParams{
			PrincipalID: "p-dl3", LegalEntityID: itEntity, ActionType: "PAYMENT_RELEASE",
			Outcome: outcome, Basis: "no_grant", CorrelationID: "it-dl3", TenantID: tenant,
			Decision: decision, ReasonCodes: []string{"NO_GRANT"},
		})
		if err != nil {
			t.Fatalf("record %s: %v", decision, err)
		}
		return d.AccessDecisionID
	}
	cases := []struct {
		decision, outcome string
		queued            int
	}{
		{domain.CanonicalDecisionDeny, domain.OutcomeDenied, 1},
		{domain.CanonicalDecisionRequireApproval, domain.OutcomeDenied, 1},
		{"", domain.OutcomeDenied, 1}, // an admin-gate denial
		{domain.CanonicalDecisionPermit, domain.OutcomeGranted, 0},
		{domain.CanonicalDecisionStepUp, domain.OutcomeDenied, 0},
	}
	var denyID string
	for _, c := range cases {
		id := record(c.decision, c.outcome)
		if got := ledgerRows(t, pool, tenant, id); got != c.queued {
			t.Fatalf("%q/%s: %d ledger rows, want %d", c.decision, c.outcome, got, c.queued)
		}
		if c.decision == domain.CanonicalDecisionDeny {
			denyID = id
		}
	}

	// The record names the decision, the original principal and the basis.
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
	var outcome, action, onBehalf, basis string
	if err := tx.QueryRow(ctx, `SELECT record->>'outcome', record->>'action_type', record->'evaluation_context'->>'on_behalf_of', record->>'rule_basis'
		FROM decision_log_outbox WHERE decision_id = $1`, denyID).Scan(&outcome, &action, &onBehalf, &basis); err != nil {
		t.Fatal(err)
	}
	if outcome != "DENY" || action != "ACCESS_PAYMENT_RELEASE" || onBehalf != "p-dl3" || basis == "" {
		t.Fatalf("record: outcome %q action %q on_behalf_of %q basis %q", outcome, action, onBehalf, basis)
	}

	// Another tenant does not see it; the relay does.
	if n := ledgerRows(t, pool, uuid.NewString(), denyID); n != 0 {
		t.Fatalf("another tenant sees %d rows", n)
	}
	b, err := decisionledger.NewPgQueue(pool).Backlog(ctx)
	if err != nil || b.Pending < 3 {
		t.Fatalf("relay backlog %+v %v", b, err)
	}
}

// The ledger record shares the decision's transaction: when it cannot be
// written, the decision is not recorded either (and the caller answers 503,
// never an unlogged denial).
func TestDL3_TheLedgerRecordCommitsWithTheDecision(t *testing.T) {
	pool := outboxPool(t)
	s := store.New(pool, zap.NewNop())
	s.SetDecisionLedger(func(domain.AccessDecisionLog) (*decisionledger.Record, error) {
		return nil, errors.New("ledger record could not be built")
	})
	before := countDecisions(t, pool, "it-dl3-rollback")
	_, err := s.RecordAccessDecision(context.Background(), domain.RecordAccessDecisionParams{
		PrincipalID: "p-dl3", LegalEntityID: itEntity, ActionType: "X", Outcome: domain.OutcomeDenied,
		Basis: "no_grant", CorrelationID: "it-dl3-rollback", TenantID: itTenant, Decision: domain.CanonicalDecisionDeny,
	})
	if err == nil {
		t.Fatal("record succeeded although its ledger record could not be written")
	}
	if after := countDecisions(t, pool, "it-dl3-rollback"); after != before {
		t.Fatalf("decision rows %d -> %d: the decision committed without its ledger record", before, after)
	}
}
