package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/procurement-workflow-svc/internal/domain"
	svcmiddleware "zoiko.io/procurement-workflow-svc/internal/middleware"
	"zoiko.io/procurement-workflow-svc/internal/outbox"
	"zoiko.io/procurement-workflow-svc/internal/store"
)

// storePool returns the pool the store runs on — the ordinary app role when
// TEST_APP_DATABASE_URL names one, so FORCE RLS is really exercised — and the
// owner pool used for migrations and assertions.
func storePool(t *testing.T) (app, owner *pgxpool.Pool) {
	t.Helper()
	owner = openTestPool(t)
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		return owner, owner
	}
	requireThrowawayDatabase(t, dsn)
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect as app role: %v", err)
	}
	t.Cleanup(p.Close)
	return p, owner
}

func newCase(tenantID string, status domain.CaseStatus) *domain.ProcurementCase {
	now := time.Now().UTC()
	return &domain.ProcurementCase{
		CaseID: uuid.NewString(), TenantID: tenantID, LegalEntityID: "le-1",
		RequestedByPrincipalID: "requester-1", Description: "d", Category: "IT",
		Amount: 250, CurrencyCode: "GBP", Status: status,
		SpendCheckDecision: "ALLOWED", SpendCheckBasis: "within_threshold",
		CorrelationID: "corr-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now,
	}
}

func outboxTypes(t *testing.T, owner *pgxpool.Pool, tenantID string) []string {
	t.Helper()
	rows, err := owner.Query(context.Background(), `SELECT event_type, payload, actor_id FROM outbox_events WHERE tenant_id = $1`, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var et, actor string
		var payload []byte
		if err := rows.Scan(&et, &payload, &actor); err != nil {
			t.Fatal(err)
		}
		var env map[string]any
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatal(err)
		}
		if env["schema_version"] != "1.0" || env["event_type"] != et || env["tenant_id"] != tenantID || env["source_service"] != "procurement-workflow-svc" {
			t.Errorf("%s envelope: %v", et, env)
		}
		out = append(out, et+"/"+actor)
	}
	sort.Strings(out)
	return out
}

// A fresh create writes requested (+ approval.started when pending) in the
// insert's transaction; a replayed create writes nothing; completion writes
// procurement.completed with the issuer as actor.
func TestOutbox_CreateAndComplete_WriteEventsInTheSameTransaction(t *testing.T) {
	app, owner := storePool(t)
	s := store.New(app)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	pc := newCase(tenantID, domain.CaseStatusApprovalPending)
	if created, err := s.CreateCase(ctx, pc); err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	replay := *pc
	replay.CaseID = uuid.NewString()
	if created, err := s.CreateCase(ctx, &replay); err != nil || created {
		t.Fatalf("replayed create: %v %v", created, err)
	}
	if got := strings.Join(outboxTypes(t, owner, tenantID), ","); got != "procurement.approval.started/requester-1,procurement.requested/requester-1" {
		t.Fatalf("after create + replay: %s", got)
	}

	blocked := newCase(tenantID, domain.CaseStatusSpendBlocked)
	if _, err := s.CreateCase(ctx, blocked); err != nil {
		t.Fatal(err)
	}
	if n := len(outboxTypes(t, owner, tenantID)); n != 3 {
		t.Fatalf("a spend-blocked case emits requested only: %d rows", n)
	}

	if _, err := s.UpdateApproved(ctx, pc.CaseID, "approver-1"); err != nil {
		t.Fatal(err)
	}
	done, err := s.UpdateOrderIssued(ctx, pc.CaseID, "po-1", "issuer-1")
	if err != nil || done.Status != domain.CaseStatusCompleted {
		t.Fatalf("complete: %+v %v", done, err)
	}
	got := outboxTypes(t, owner, tenantID)
	if len(got) != 4 || !strings.Contains(strings.Join(got, ","), "procurement.completed/issuer-1") {
		t.Fatalf("completion must write procurement.completed by the issuer: %v", got)
	}
	// A second completion is an invalid transition and writes nothing.
	if _, err := s.UpdateOrderIssued(ctx, pc.CaseID, "po-1", "issuer-1"); err == nil {
		t.Fatal("completing twice must fail")
	}
	if n := len(outboxTypes(t, owner, tenantID)); n != 4 {
		t.Fatalf("a refused transition wrote an event: %d rows", n)
	}
}

// Same transaction: an outbox failure rolls back the case insert, and the
// COMPLETED transition — neither the change nor the event exists.
func TestOutbox_InsertFailure_RollsBackTheCaseChange(t *testing.T) {
	app, owner := storePool(t)
	s := store.New(app)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	add := func(name, check string) {
		if _, err := owner.Exec(context.Background(), `ALTER TABLE outbox_events ADD CONSTRAINT `+name+` CHECK (`+check+`)`); err != nil {
			t.Fatal(err)
		}
	}
	drop := func(name string) {
		if _, err := owner.Exec(context.Background(), `ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS `+name); err != nil {
			t.Fatal(err)
		}
	}

	// Second insert of the create (approval.started) fails: the case and the
	// already-inserted requested row both roll back.
	add("test_reject_started", "event_type <> 'procurement.approval.started'")
	pc := newCase(tenantID, domain.CaseStatusApprovalPending)
	if _, err := s.CreateCase(ctx, pc); err == nil {
		t.Fatal("a failed outbox write must fail the create")
	}
	drop("test_reject_started")
	var n int
	if err := owner.QueryRow(context.Background(), `SELECT count(*) FROM procurement_cases WHERE case_id = $1`, pc.CaseID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the case survived a failed outbox write: %d %v", n, err)
	}
	if got := outboxTypes(t, owner, tenantID); len(got) != 0 {
		t.Fatalf("outbox rows survived the rolled-back create: %v", got)
	}

	// Completion: the event fails, the case stays APPROVED.
	pc = newCase(tenantID, domain.CaseStatusApprovalPending)
	if _, err := s.CreateCase(ctx, pc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateApproved(ctx, pc.CaseID, "approver-1"); err != nil {
		t.Fatal(err)
	}
	add("test_reject_completed", "event_type <> 'procurement.completed'")
	t.Cleanup(func() { drop("test_reject_completed") })
	if _, err := s.UpdateOrderIssued(ctx, pc.CaseID, "po-1", "issuer-1"); err == nil {
		t.Fatal("a failed outbox write must fail the completion")
	}
	cur, err := s.GetCase(ctx, pc.CaseID)
	if err != nil || cur.Status != domain.CaseStatusApproved || cur.PurchaseOrderID != nil {
		t.Fatalf("the COMPLETED transition survived a failed outbox write: %+v %v", cur, err)
	}
}

type capturePub struct {
	mu   sync.Mutex
	got  map[string][]byte
	fail error
}

func (c *capturePub) PublishOutbox(_ context.Context, id, _ string, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	if c.got == nil {
		c.got = map[string][]byte{}
	}
	c.got[id] = payload
	return nil
}

// The relay drains every tenant's rows (app.outbox_relay), keeps a failed
// publish pending for retry, and marks delivered rows published.
func TestOutbox_RelayPublishesAcrossTenants_RetriesFailures(t *testing.T) {
	app, owner := storePool(t)
	s := store.New(app)
	for i := 0; i < 2; i++ {
		tenantID := uuid.NewString()
		if _, err := s.CreateCase(svcmiddleware.WithTenant(context.Background(), tenantID), newCase(tenantID, domain.CaseStatusApprovalPending)); err != nil {
			t.Fatal(err)
		}
	}
	pub := &capturePub{fail: errors.New("broker down")}
	relay := outbox.NewRelay(app, pub, time.Second, 100, zap.NewNop())
	if n := relay.RelayOnce(context.Background()); n != 0 {
		t.Fatalf("a failing broker delivered %d", n)
	}
	var pending int
	if err := owner.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND publish_attempts = 1`).Scan(&pending); err != nil || pending != 4 {
		t.Fatalf("failed rows must stay pending with one attempt: %d %v", pending, err)
	}
	pub.fail = nil
	if n := relay.RelayOnce(context.Background()); n != 4 {
		t.Fatalf("relay delivered %d of 4 across two tenants", n)
	}
	if err := owner.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("%d rows left unpublished (%v)", pending, err)
	}
}

// Under the app role, an unscoped session sees no outbox row and a tenant
// cannot write another tenant's row.
func TestOutbox_RLS_UnscopedSeesNothing_CrossTenantInsertRefused(t *testing.T) {
	if os.Getenv("TEST_APP_DATABASE_URL") == "" {
		t.Skip("TEST_APP_DATABASE_URL not set: needs an ordinary role for RLS")
	}
	app, _ := storePool(t)
	s := store.New(app)
	tenantID := uuid.NewString()
	if _, err := s.CreateCase(svcmiddleware.WithTenant(context.Background(), tenantID), newCase(tenantID, domain.CaseStatusApprovalPending)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := app.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unscoped: RLS must hide every outbox row, saw %d (%v)", n, err)
	}
	tx, err := app.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(context.Background(), `
		INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, tenant_id, legal_entity_id, payload)
		VALUES ('x', 'y', 'Forged', $1, 'le', '{}')`, uuid.NewString())
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("writing another tenant's outbox row must be refused by RLS, got %v", err)
	}
}
