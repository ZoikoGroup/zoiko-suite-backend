package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/data-ingestion-svc/internal/outbox"
)

type fakePublisher struct {
	mu   sync.Mutex
	seen []string
	fail map[string]bool
}

func (f *fakePublisher) PublishForOutbox(_ context.Context, eventType, entityID, tenantID string, _ interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[eventType] {
		return errors.New("broker down")
	}
	f.seen = append(f.seen, tenantID+"|"+eventType+"|"+entityID)
	return nil
}

func (f *fakePublisher) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.seen) }

// The relay reads events across every tenant, and outbox_events is row-level secured by tenant. Run
// as an unprivileged role it must still see and publish them: found in a live run, where it saw
// nothing and no event was ever published.
func TestOutboxRelay_PublishesAsAnUnprivilegedRole(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(plainSpec())
	b := e.approved(p, csvOf(goodRows(2, entityOne)...), declared{}) // emits import.validated and import.preview-approved

	var pending int
	iMust(t, e.admin.QueryRow(ictx, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND published_at IS NULL`, e.tenant).Scan(&pending))
	if pending < 2 {
		t.Fatalf("expected the batch's events to be waiting, found %d", pending)
	}
	pub := &fakePublisher{fail: map[string]bool{"import.preview-approved": true}}
	relay := outbox.NewRelay(e.app, pub, 0, 500, zap.NewNop())
	relay.RunOnce(ictx)

	var published, failedAttempts int
	iMust(t, e.admin.QueryRow(ictx, `SELECT count(*) FILTER (WHERE published_at IS NOT NULL), coalesce(sum(publish_attempts), 0)
		FROM outbox_events WHERE tenant_id = $1`, e.tenant).Scan(&published, &failedAttempts))
	if published == 0 || pub.count() == 0 {
		t.Fatalf("the relay published nothing as an unprivileged role (published=%d)", published)
	}
	if failedAttempts != 1 {
		t.Fatalf("a failed publish must be counted and left for the next pass: attempts=%d", failedAttempts)
	}
	// the failed one is retried and then published; nothing is published twice
	pub.mu.Lock()
	pub.fail = map[string]bool{}
	pub.mu.Unlock()
	relay.RunOnce(ictx)
	relay.RunOnce(ictx)
	var left int
	iMust(t, e.admin.QueryRow(ictx, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND published_at IS NULL`, e.tenant).Scan(&left))
	if left != 0 {
		t.Fatalf("%d events are still unpublished", left)
	}
	seen := map[string]int{}
	for _, s := range pub.seen {
		seen[s]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s was published %d times", k, n)
		}
	}
	_ = b
}

// The relay's flag admits it to outbox rows only: it does not open the tenant tables.
func TestOutboxRelay_FlagOpensOnlyTheOutbox(t *testing.T) {
	e := newImpEnv(t)
	e.approved(e.profile(plainSpec()), csvOf(goodRows(1, entityOne)...), declared{})
	count := func(table string) int {
		tx, err := e.app.Begin(ictx)
		iMust(t, err)
		defer tx.Rollback(ictx) //nolint:errcheck
		_, err = tx.Exec(ictx, "SELECT set_config('app.outbox_relay', 'true', true)")
		iMust(t, err)
		var n int
		iMust(t, tx.QueryRow(ictx, "SELECT count(*) FROM "+table).Scan(&n))
		return n
	}
	if count("outbox_events") == 0 {
		t.Error("the relay flag must expose outbox rows")
	}
	for _, table := range []string{"import_batches", "import_items", "import_chunks", "import_batch_log", "ingestion_runs", "landed_records"} {
		if n := count(table); n != 0 {
			t.Errorf("%s: the relay flag exposed %d rows", table, n)
		}
	}
}
