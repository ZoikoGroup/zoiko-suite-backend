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

	"zoiko.io/contract-lifecycle-svc/internal/domain"
	"zoiko.io/contract-lifecycle-svc/internal/outbox"
	"zoiko.io/contract-lifecycle-svc/internal/store"
)

func outboxTypes(t *testing.T, pool *pgxpool.Pool, contractID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT event_type FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at, outbox_event_id`, contractID)
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

// effective drives a new contract to EFFECTIVE with distinct actors.
func effective(t *testing.T, s *store.PgStore, ctx context.Context) *domain.Contract {
	t.Helper()
	c := newContract("le-us", "drafter-1")
	steps := []func() error{
		func() error {
			return s.CreateContract(ctx, c, store.Audit{ActorID: "drafter-1", CorrelationID: "corr-1"})
		},
		func() error {
			_, err := s.SubmitReview(ctx, c.ContractID, store.Audit{ActorID: "drafter-1"})
			return err
		},
		func() error {
			_, err := s.ApproveContract(ctx, c.ContractID, "dec-001", store.Audit{ActorID: "legal-lead-1"})
			return err
		},
		func() error {
			_, err := s.SendForSignature(ctx, c.ContractID, store.Audit{ActorID: "legal-lead-1"})
			return err
		},
		func() error {
			_, err := s.RecordExecution(ctx, c.ContractID, &domain.RecordExecutionRequest{SignedBy: "Jane Counterparty"}, store.Audit{ActorID: "recorder-1"})
			return err
		},
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	return c
}

// Every write emits exactly its event, in the same transaction, carrying the
// envelope the direct publish used to send (now with schema_version and the
// outbox id as a stable event_id).
func TestOutbox_EveryWriteEmitsItsEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")
	c := effective(t, s, ctx)

	want := []string{"contract.created", "contract.submitted_for_review", "contract.approved", "contract.sent_for_signature", "contract.executed"}
	got := outboxTypes(t, pool, c.ContractID)
	if len(got) != len(want) {
		t.Fatalf("outbox events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("outbox events = %v, want %v", got, want)
		}
	}

	var id, tenant, actor string
	var payload []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT outbox_event_id::text, tenant_id, actor_id, payload FROM outbox_events WHERE aggregate_id=$1 AND event_type='contract.created'`,
		c.ContractID).Scan(&id, &tenant, &actor, &payload); err != nil {
		t.Fatalf("read created event: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if env["event_id"] != id || env["event_type"] != "contract.created" || env["schema_version"] != "1.0" ||
		env["contract_id"] != c.ContractID || env["tenant_id"] != "tenant-a" || env["actor_id"] != "drafter-1" ||
		env["correlation_id"] != "corr-1" || env["source_service"] != "contract-lifecycle-svc" {
		t.Fatalf("unexpected envelope: %v", env)
	}
	if tenant != "tenant-a" || actor != "drafter-1" {
		t.Fatalf("row tenant/actor = %s/%s", tenant, actor)
	}
}

// The actor columns record the authenticated principal handed in as Audit,
// never a body value; signed_by stays the signing evidence.
func TestActorColumns_RecordThePrincipal(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")
	c := effective(t, s, ctx)

	if _, err := s.AmendContract(ctx, c.ContractID, &domain.AmendContractRequest{AmendedBy: "forged", TotalValue: 1}, store.Audit{ActorID: "amender-1"}); err != nil {
		t.Fatalf("amend: %v", err)
	}
	if _, err := s.RenewContract(ctx, c.ContractID, &domain.RenewContractRequest{RenewedBy: "forged", NewEffectiveTo: "2030-01-01"}, store.Audit{ActorID: "renewer-1"}); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if _, err := s.TerminateContract(ctx, c.ContractID, &domain.TerminateContractRequest{TerminatedBy: "forged"}, store.Audit{ActorID: "terminator-1"}); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	got, err := s.GetContract(ctx, c.ContractID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	check := func(name string, p *string, want string) {
		if p == nil || *p != want {
			t.Errorf("%s = %v, want %s", name, p, want)
		}
	}
	check("amended_by", got.AmendedBy, "amender-1")
	check("renewed_by", got.RenewedBy, "renewer-1")
	check("terminated_by", got.TerminatedBy, "terminator-1")
	check("signed_by", got.SignedBy, "Jane Counterparty")
	check("execution_recorded_by", got.ExecutionRecordedBy, "recorder-1")
}

// The outbox row and the business change share one transaction: if the event
// cannot be written, the transition is rolled back too, and vice versa a
// refused transition writes no event.
func TestOutbox_SameTransaction_RollbackLeavesNeither(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	c := newContract("le-us", "drafter-1")
	if err := s.CreateContract(ctx, c, store.Audit{ActorID: "drafter-1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SubmitReview(ctx, c.ContractID, store.Audit{ActorID: "drafter-1"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Refused transition (self-approval): no event.
	if _, err := s.ApproveContract(ctx, c.ContractID, "dec-001", store.Audit{ActorID: "drafter-1"}); !errors.Is(err, domain.ErrSelfApprovalNotAllowed) {
		t.Fatalf("self-approve: %v", err)
	}
	if n := len(outboxTypes(t, pool, c.ContractID)); n != 2 {
		t.Fatalf("refused approval wrote an event: %d rows", n)
	}

	// Make the outbox insert for contract.approved fail: the approval itself
	// must then not commit.
	if _, err := pool.Exec(context.Background(),
		`ALTER TABLE outbox_events ADD CONSTRAINT test_block_approved CHECK (event_type <> 'contract.approved')`); err != nil {
		t.Fatalf("add blocking constraint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS test_block_approved`)
	})
	if _, err := s.ApproveContract(ctx, c.ContractID, "dec-001", store.Audit{ActorID: "legal-lead-1"}); err == nil {
		t.Fatal("approve succeeded although its event could not be written")
	}
	got, err := s.GetContract(ctx, c.ContractID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.ContractStatusReview || got.ApprovedBy != nil {
		t.Fatalf("approval committed without its event: status=%s approved_by=%v", got.Status, got.ApprovedBy)
	}
	if n := len(outboxTypes(t, pool, c.ContractID)); n != 2 {
		t.Fatalf("outbox rows = %d, want 2", n)
	}
}

type fakePub struct {
	fail bool
	got  []string
}

func (p *fakePub) PublishOutbox(_ context.Context, id, _ string, _ []byte) error {
	if p.fail {
		return errors.New("broker down")
	}
	p.got = append(p.got, id)
	return nil
}

func TestOutboxRelay_DeliversAndRetries(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	c := newContract("le-us", "drafter-1")
	if err := s.CreateContract(tenantCtx("tenant-a"), c, store.Audit{ActorID: "drafter-1"}); err != nil {
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
		t.Fatalf("attempts=%d unpublished=%d, want 1/1 (kept for retry)", attempts, unpublished)
	}
	pub.fail = false
	if n := relay.RelayOnce(context.Background()); n != 1 {
		t.Fatalf("delivered %d, want 1", n)
	}
	if n := relay.RelayOnce(context.Background()); n != 0 {
		t.Fatalf("published rows re-sent: %d", n)
	}
}

// appPool connects as an ordinary NOSUPERUSER NOBYPASSRLS role, so the
// outbox_events policy actually binds (a superuser bypasses RLS).
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

// Under FORCE RLS an unscoped session sees no outbox row, the relay (as the
// service role, declaring app.outbox_relay) drains every tenant, and a
// request-path insert for another tenant is refused.
func TestOutboxRLS_AsAppRole(t *testing.T) {
	pool := openTestPool(t)
	app := appPool(t)
	s := store.NewPgStore(app)
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if err := s.CreateContract(tenantCtx(tenant), newContract("le-us", "drafter-1"), store.Audit{ActorID: "drafter-1"}); err != nil {
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
	relay := outbox.NewRelay(app, &fakePub{}, time.Second, 50, zap.NewNop())
	if n := relay.RelayOnce(context.Background()); n != 2 {
		t.Fatalf("relay delivered %d, want 2 (both tenants)", n)
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
		AggregateType: "contract", AggregateID: "x", EventType: "Forged", TenantID: "tenant-b", Payload: map[string]string{},
	}); err == nil {
		t.Fatal("cross-tenant outbox insert was accepted")
	}
	_ = pool
}
