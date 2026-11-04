package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/clause-template-svc/internal/domain"
	"zoiko.io/clause-template-svc/internal/middleware"
	"zoiko.io/clause-template-svc/internal/outbox"
	"zoiko.io/clause-template-svc/internal/store"
)

func outboxTypes(t *testing.T, pool *pgxpool.Pool, aggregateID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT event_type FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at, outbox_event_id`, aggregateID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Every write emits exactly its event in its own transaction, carrying the
// envelope the direct publish used to send.
func TestOutbox_EveryWriteEmitsItsEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	c := newClause("le-us", "drafter-1")
	if err := s.CreateClause(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	c.Title = "edited"
	if err := s.UpdateClause(ctx, c, "edit"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.SubmitClauseForLegalReview(ctx, c.ClauseID, "drafter-1"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.ApproveClause(ctx, c.ClauseID, "lead-1"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := s.ActivateClause(ctx, c.ClauseID, "lead-1"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if _, err := s.RetireClause(ctx, c.ClauseID, "lead-1"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	want := []string{"clause.created", "clause.updated", "clause.submitted_for_legal_review", "clause.version_approved", "clause.activated", "clause.retired"}
	if got := outboxTypes(t, pool, c.ClauseID); !equal(got, want) {
		t.Fatalf("clause events = %v, want %v", got, want)
	}

	tmpl := &domain.ContractTemplate{LegalEntityID: "le-us", Title: "NDA", ContractType: "NDA", CreatedBy: "drafter-1", EffectiveFrom: "2026-01-01"}
	if err := s.CreateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("create template: %v", err)
	}
	if err := s.UpdateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("update template: %v", err)
	}
	if _, err := s.ApproveTemplate(ctx, tmpl.TemplateID, "lead-1"); err != nil {
		t.Fatalf("approve template: %v", err)
	}
	if got, want := outboxTypes(t, pool, tmpl.TemplateID), []string{"template.created", "template.updated", "template.approved"}; !equal(got, want) {
		t.Fatalf("template events = %v, want %v", got, want)
	}

	d := &domain.DeviationRule{LegalEntityID: "le-us", JurisdictionID: "us-de", RiskClassification: domain.RiskHigh, Description: "x", ProposedBy: "lawyer-1"}
	if err := s.CreateDeviationRule(ctx, d); err != nil {
		t.Fatalf("create deviation: %v", err)
	}
	if _, err := s.ApproveDeviationRule(ctx, d.DeviationID, "lead-1"); err != nil {
		t.Fatalf("approve deviation: %v", err)
	}
	if got, want := outboxTypes(t, pool, d.DeviationID), []string{"deviation_rule.created", "deviation_rule.changed"}; !equal(got, want) {
		t.Fatalf("deviation events = %v, want %v", got, want)
	}

	var id string
	var payload []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT outbox_event_id::text, payload FROM outbox_events WHERE aggregate_id=$1 AND event_type='clause.created'`,
		c.ClauseID).Scan(&id, &payload); err != nil {
		t.Fatalf("read event: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if env["event_id"] != "evt-"+id || env["schema_version"] != "1.0" || env["entity_id"] != c.ClauseID ||
		env["tenant_id"] != "tenant-a" || env["actor_id"] != "test-actor" || env["correlation_id"] != "corr-test" ||
		env["jurisdiction"] != "us-delaware" || env["source_service"] != "clause-template-svc" {
		t.Fatalf("unexpected envelope: %v", env)
	}
}

// A write with no attributed actor is refused, not emitted unattributed.
func TestOutbox_WriteWithoutAudit_IsRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	c := newClause("le-us", "drafter-1")
	if err := s.CreateClause(ctx, c); !errors.Is(err, store.ErrAuditMissing) {
		t.Fatalf("create without audit returned %v, want ErrAuditMissing", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM clauses`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("clauses = %d (%v), want 0", n, err)
	}
}

// The outbox row and the change share one transaction: if the event cannot
// be written the transition rolls back too; a refused transition emits nothing.
func TestOutbox_SameTransaction_RollbackLeavesNeither(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")
	c := newClause("le-us", "drafter-1")
	if err := s.CreateClause(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SubmitClauseForLegalReview(ctx, c.ClauseID, "drafter-1"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.ApproveClause(ctx, c.ClauseID, "drafter-1"); !errors.Is(err, domain.ErrSelfApprovalNotAllowed) {
		t.Fatalf("self-approve: %v", err)
	}
	if n := len(outboxTypes(t, pool, c.ClauseID)); n != 2 {
		t.Fatalf("refused approval wrote an event: %d rows", n)
	}

	if _, err := pool.Exec(context.Background(),
		`ALTER TABLE outbox_events ADD CONSTRAINT test_block CHECK (event_type <> 'clause.version_approved')`); err != nil {
		t.Fatalf("add blocking constraint: %v", err)
	}
	if _, err := s.ApproveClause(ctx, c.ClauseID, "lead-1"); err == nil {
		t.Fatal("approve succeeded although its event could not be written")
	}
	got, err := s.GetClause(ctx, c.ClauseID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.StatusLegalReview || got.ApprovedBy != nil {
		t.Fatalf("approval committed without its event: status=%s", got.Status)
	}
	if n := len(outboxTypes(t, pool, c.ClauseID)); n != 2 {
		t.Fatalf("outbox rows = %d, want 2", n)
	}
}

type fakePub struct{ fail bool }

func (p *fakePub) PublishOutbox(context.Context, string, string, []byte) error {
	if p.fail {
		return errors.New("broker down")
	}
	return nil
}

func TestOutboxRelay_DeliversAndRetries(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	if err := s.CreateClause(tenantCtx("tenant-a"), newClause("le-us", "drafter-1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	pub := &fakePub{fail: true}
	relay := outbox.NewRelay(pool, pub, time.Second, 50, zap.NewNop())
	if n := relay.RelayOnce(context.Background()); n != 0 {
		t.Fatalf("broker down: delivered %d", n)
	}
	var attempts, unpublished int
	if err := pool.QueryRow(context.Background(),
		`SELECT max(publish_attempts), count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&attempts, &unpublished); err != nil {
		t.Fatalf("read: %v", err)
	}
	if attempts != 1 || unpublished != 1 {
		t.Fatalf("attempts=%d unpublished=%d, want 1/1", attempts, unpublished)
	}
	pub.fail = false
	if n := relay.RelayOnce(context.Background()); n != 1 {
		t.Fatalf("delivered %d, want 1", n)
	}
	if n := relay.RelayOnce(context.Background()); n != 0 {
		t.Fatalf("published rows re-sent: %d", n)
	}
}

func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_APP_DATABASE_URL not set: needs an ordinary role for RLS")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect app role: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// As a NOSUPERUSER NOBYPASSRLS role: an unscoped session sees no outbox row,
// the relay drains every tenant, a cross-tenant insert is refused.
func TestOutboxRLS_AsAppRole(t *testing.T) {
	_ = openTestPool(t)
	app := appPool(t)
	s := store.NewPgStore(app)
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if err := s.CreateClause(tenantCtx(tenant), newClause("le-us", "drafter-1")); err != nil {
			t.Fatalf("create as app role (%s): %v", tenant, err)
		}
	}
	var visible int
	if err := app.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&visible); err != nil {
		t.Fatalf("count: %v", err)
	}
	if visible != 0 {
		t.Fatalf("unscoped app session sees %d outbox rows, want 0", visible)
	}
	if n := outbox.NewRelay(app, &fakePub{}, time.Second, 50, zap.NewNop()).RelayOnce(context.Background()); n != 2 {
		t.Fatalf("relay delivered %d, want 2", n)
	}
	tx, err := app.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'tenant-a', true)`); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if _, err := outbox.Insert(context.Background(), tx, outbox.Event{
		AggregateType: "clause", AggregateID: "x", EventType: "Forged", TenantID: "tenant-b", Payload: map[string]string{},
	}); err == nil {
		t.Fatal("cross-tenant outbox insert was accepted")
	}
}
