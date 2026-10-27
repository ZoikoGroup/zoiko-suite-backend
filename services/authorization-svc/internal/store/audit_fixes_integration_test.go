package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/events"
	"zoiko.io/authorization-svc/internal/outbox"
)

// Governance audit fixes of 13 Oct 2026 on a real database (AUTHZ_IT_DSN,
// migrated to 000031, as a NOBYPASSRLS role).

// N-02: a suspended or terminated delegator's authority stops flowing through
// their delegations — on every query that confers or attributes it — and
// flows again when they are reinstated.
func TestFixesIT_SuspendedDelegatorConfersNothing(t *testing.T) {
	f := newFixture(t)
	entity := uuid.NewString()
	r := f.role(t, "payment.approve")
	f.assign(t, "boss-n02", r.RoleID, domain.ApprovalApproved)
	end := time.Now().Add(24 * time.Hour)
	if _, err := f.s.CreateDelegatedAuthority(f.ctx, domain.CreateDelegatedAuthorityParams{TenantID: f.tenant, DelegatorPrincipalID: "boss-n02",
		DelegatePrincipalID: "cover-n02", ScopeType: "FULL", EffectiveFrom: time.Now().Add(-time.Minute), EffectiveTo: &end}); err != nil {
		t.Fatal(err)
	}

	confers := func() (actions []string, delegator string, ceilings int) {
		t.Helper()
		actions, _, err := f.s.FindDelegatedActionsScoped(f.ctx, "cover-n02", entity, f.tenant, "", "")
		if err != nil {
			t.Fatal(err)
		}
		delegator, _, err = f.s.FindDelegationSource(f.ctx, "cover-n02", entity, f.tenant, "", "", "payment.approve")
		if err != nil {
			t.Fatal(err)
		}
		c, err := f.s.FindDelegationCeilings(f.ctx, "cover-n02", entity, f.tenant, "payment.approve")
		if err != nil {
			t.Fatal(err)
		}
		return actions, delegator, len(c)
	}
	setStatus := func(status string) {
		t.Helper()
		at := time.Now().UTC()
		if _, err := f.s.ProjectPrincipalStatus(f.ctx, domain.ProjectPrincipalStatusParams{PrincipalID: "boss-n02", TenantID: f.tenant,
			Status: status, SourceService: "identity-context-svc", StatusChangedAt: &at}); err != nil {
			t.Fatal(err)
		}
	}

	if a, d, c := confers(); len(a) != 1 || d != "boss-n02" || c != 1 {
		t.Fatalf("active delegator: actions=%v delegator=%q ceilings=%d, want the delegation to confer", a, d, c)
	}
	for _, status := range []string{"SUSPENDED", "TERMINATED"} {
		setStatus(status)
		if a, d, c := confers(); len(a) != 0 || d != "" || c != 0 {
			t.Errorf("%s delegator: actions=%v delegator=%q ceilings=%d, want nothing conferred", status, a, d, c)
		}
	}
	setStatus("ACTIVE")
	if a, _, _ := confers(); len(a) != 1 {
		t.Errorf("reinstated delegator: actions=%v, want the delegation to confer again", a)
	}
}

// countingWriter counts WriteMessages calls and can fail chosen keys.
type countingWriter struct {
	calls   int
	sent    []kafka.Message
	failKey string
}

func (w *countingWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	w.calls++
	if w.failKey == "" {
		w.sent = append(w.sent, msgs...)
		return nil
	}
	errs := make(kafka.WriteErrors, len(msgs))
	for i, m := range msgs {
		if string(m.Key) == w.failKey {
			errs[i] = errors.New("partition leader unavailable")
			continue
		}
		w.sent = append(w.sent, m)
	}
	return errs
}

// N-04: one batch is one Kafka write, and a partial failure leaves only the
// failed rows pending.
func TestFixesIT_RelayWritesABatchAtOnce(t *testing.T) {
	pool := outboxPool(t)
	f := newFixture(t)
	drain := outbox.NewRelay(pool, &countingWriter{}, 0, 100000, zap.NewNop())
	if _, err := drain.RelayOnce(context.Background()); err != nil { // clear rows other tests left pending
		t.Fatal(err)
	}

	var ids []string
	for i := 0; i < 150; i++ {
		d, err := f.s.RecordAccessDecision(f.ctx, domain.RecordAccessDecisionParams{PrincipalID: "p-n04", LegalEntityID: uuid.NewString(),
			ActionType: "X", Outcome: "DENIED", Basis: "no_grant", CorrelationID: "n04", TenantID: f.tenant, Events: events.DecisionEvents})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.AccessDecisionID)
	}

	w := &countingWriter{failKey: ids[7]}
	n, err := outbox.NewRelay(pool, w, 0, 1000, zap.NewNop()).RelayOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w.calls != 1 {
		t.Errorf("WriteMessages called %d times for one batch, want 1", w.calls)
	}
	// The relay claims every pending row, not only this test's: the package
	// suites run in parallel against one database, and another package's
	// integration test can publish a row of its own into this batch. Count
	// the events this test recorded, not the batch (which flaked as
	// "published 150, want 149" whenever a foreign row slipped in).
	ours := make(map[string]bool, len(ids))
	for _, id := range ids {
		ours[id] = true
	}
	published := 0
	for _, m := range w.sent {
		if ours[string(m.Key)] {
			published++
		}
	}
	if n < 149 {
		t.Errorf("relay reported %d published, want at least this test's 149", n)
	}
	if published != 149 {
		t.Errorf("published %d of this test's 150 events, want 149 (one failed)", published)
	}
	if got := pendingFor(t, pool, ids[7]); len(got) != 1 {
		t.Errorf("the failed row is not pending: %v", got)
	}
	if got := pendingFor(t, pool, ids[8]); len(got) != 0 {
		t.Errorf("a published row is still pending: %v", got)
	}
}

// N-14: published outbox rows and idempotency keys past their window are
// purged across tenants; pending rows and recent ones stay; and the purge
// policy cannot be borrowed by an ordinary session.
func TestFixesIT_PurgeFunctions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	tag := "n14-" + uuid.NewString()[:8]
	exec(`INSERT INTO outbox_events (event_type, message_key, message_value, published_at) VALUES
		('x', $1||'-old', '{}', NOW() - INTERVAL '10 days'),
		('x', $1||'-recent', '{}', NOW() - INTERVAL '1 day'),
		('x', $1||'-pending', '{}', NULL)`, tag)
	exec(`UPDATE outbox_events SET created_at = NOW() - INTERVAL '10 days' WHERE message_key = $1||'-pending'`, tag)

	if _, err := f.s.PurgePublishedOutbox(ctx, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var left []string
	rows, err := f.pool.Query(ctx, `SELECT message_key FROM outbox_events WHERE message_key LIKE $1||'%' ORDER BY 1`, tag)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		left = append(left, strings.TrimPrefix(k, tag+"-"))
	}
	rows.Close()
	if strings.Join(left, ",") != "pending,recent" {
		t.Errorf("outbox after purge = %v, want pending and recent kept, old published gone", left)
	}
	if _, err := f.s.PurgePublishedOutbox(ctx, time.Minute); err == nil {
		t.Error("a sub-hour retention was accepted")
	}

	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	insertKey := func(tenant, key, age string) {
		t.Helper()
		err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (tenant_id, endpoint, idempotency_key, request_fingerprint, response_status, created_at)
				VALUES ($1, 'POST /x', $2, repeat('a', 64), 201, NOW() - $3::interval)`, tenant, key, age)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	insertKey(tenantA, "a-old", "10 days")
	insertKey(tenantB, "b-old", "10 days")
	insertKey(tenantB, "b-new", "1 hour")

	// An ordinary session cannot borrow the purge policy: it sets the flag
	// and asks to delete another tenant's key, and nothing goes.
	var stolen int64
	err = pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.idempotency_purge', 'on', true)`, tenantA); err != nil {
			return err
		}
		tagDel, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE tenant_id = $1`, tenantB)
		stolen = tagDel.RowsAffected()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if stolen != 0 {
		t.Fatalf("a plain session deleted %d of another tenant's keys by setting the purge flag", stolen)
	}

	if _, err := f.s.PurgeIdempotencyKeys(ctx, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	countKeys := func(tenant string) int {
		t.Helper()
		var n int
		err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1`, tenant).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if a, b := countKeys(tenantA), countKeys(tenantB); a != 0 || b != 1 {
		t.Errorf("after purge: tenant A %d keys (want 0), tenant B %d (want 1, the recent one)", a, b)
	}
}

// N-09: workload bindings can be written, listed and switched off, each write
// with its event in the same transaction, and a bound workload is known as
// one whichever tenant asks.
func TestFixesIT_WorkloadBindings(t *testing.T) {
	f := newFixture(t)
	id := "wl-" + uuid.NewString()[:8]
	ev := func(kind string) domain.OutboxMessage {
		return domain.OutboxMessage{EventType: kind, Key: id, Value: []byte(`{"workload_id":"` + id + `"}`), TenantID: f.tenant}
	}
	if bound, err := f.s.IsBoundWorkload(f.ctx, id); err != nil || bound {
		t.Fatalf("before any binding: bound=%v err=%v", bound, err)
	}
	saved, err := f.s.SaveWorkloadBinding(f.ctx, domain.WorkloadBinding{WorkloadID: id, TenantID: f.tenant,
		AllowedAudience: "ledger-api", AllowedActions: []string{"GL_JOURNAL_READ"}}, ev("iam.workload_binding.saved"))
	if err != nil || !saved.ActiveFlag || saved.TenantID != f.tenant {
		t.Fatalf("save: %+v %v", saved, err)
	}
	got, err := f.s.FindWorkloadBinding(f.ctx, id, f.tenant)
	if err != nil || len(got.AllowedActions) != 1 || got.AllowedActions[0] != "GL_JOURNAL_READ" {
		t.Fatalf("read back: %+v %v", got, err)
	}
	if bound, err := f.s.IsBoundWorkload(f.ctx, id); err != nil || !bound {
		t.Errorf("after binding: bound=%v err=%v", bound, err)
	}
	if list, err := f.s.ListWorkloadBindings(f.ctx, f.tenant); err != nil || len(list) != 1 {
		t.Errorf("list: %v %v", list, err)
	}
	if _, err := f.s.SetWorkloadBindingActive(f.ctx, id, f.tenant, false, ev("iam.workload_binding.deactivated")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.FindWorkloadBinding(f.ctx, id, f.tenant); !errors.Is(err, domain.ErrWorkloadBindingNotFound) {
		t.Errorf("a deactivated binding is still found: %v", err)
	}
	if _, err := f.s.SetWorkloadBindingActive(f.ctx, "nope-"+id, f.tenant, false, ev("x")); !errors.Is(err, domain.ErrWorkloadBindingNotFound) {
		t.Errorf("unknown binding: %v, want ErrWorkloadBindingNotFound", err)
	}
	// Another tenant cannot see or switch it.
	if _, err := f.s.SetWorkloadBindingActive(f.ctx, id, uuid.NewString(), true, ev("x")); !errors.Is(err, domain.ErrWorkloadBindingNotFound) {
		t.Errorf("cross-tenant reactivate: %v, want ErrWorkloadBindingNotFound", err)
	}
	if types := f.outboxTypes(t, id); strings.Join(types, ",") != "iam.workload_binding.saved,iam.workload_binding.deactivated" {
		t.Errorf("events = %v", types)
	}
}
