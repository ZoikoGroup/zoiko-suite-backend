package outbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/anomaly-detection-svc/internal/domain"
	"zoiko.io/anomaly-detection-svc/internal/events"
	"zoiko.io/anomaly-detection-svc/internal/outbox"
	"zoiko.io/anomaly-detection-svc/internal/store"
	"zoiko.io/anomaly-detection-svc/internal/testdb"
)

type recordingPublisher struct {
	mu   sync.Mutex
	fail bool
	got  []events.PublishParams
}

func (p *recordingPublisher) Publish(_ context.Context, params events.PublishParams) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return errors.New("broker down")
	}
	p.got = append(p.got, params)
	return nil
}

// The relay runs as the NOSUPERUSER NOBYPASSRLS app role: the outbox is FORCE
// RLS, and the relay is admitted only through its app.outbox_relay disjunct.
// A failed publish keeps the row pending with a retry scheduled; a later
// successful publish carries the original event_id and stamps published_at.
func TestRelay_PublishesCommittedEventsAcrossTenants(t *testing.T) {
	admin, app := testdb.Open(t, "anomaly_outbox_it")
	ctx := context.Background()
	st := store.NewPgStore(app)

	for i, tenant := range []string{"tenant-a", "tenant-b"} {
		_, err := st.DetectAnomaly(ctx, domain.CommandContext{
			TenantID: tenant, PrincipalID: "p", LegalEntityID: "le", CorrelationID: "c",
			Idempotency: domain.IdempotencyClaim{RouteTemplate: "POST /v1/anomalies/detect", Key: "k", RequestSHA256: "h"},
		}, domain.DetectAnomalyRequest{DomainName: "FIN", SourceEntityID: "tx", MetricType: "m", ObservedValue: float64(10 + i), ExpectedValue: 1})
		if err != nil {
			t.Fatalf("detect for %s: %v", tenant, err)
		}
	}

	pub := &recordingPublisher{fail: true}
	relay := outbox.NewRelay(app, pub, zap.NewNop())
	if n, err := relay.PublishBatch(ctx); err != nil || n != 0 {
		t.Fatalf("failing publish: n=%d err=%v", n, err)
	}
	var attempts int
	if err := admin.QueryRow(ctx, `SELECT COALESCE(MAX(attempts),0) FROM anomaly_outbox WHERE published_at IS NULL`).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("retry not scheduled: attempts=%d err=%v", attempts, err)
	}

	// Make the retried row due now, then publish successfully.
	if _, err := admin.Exec(ctx, `UPDATE anomaly_outbox SET available_at = NOW()`); err != nil {
		t.Fatalf("reset backoff: %v", err)
	}
	pub.fail = false
	n, err := relay.PublishBatch(ctx)
	if err != nil || n != 2 {
		t.Fatalf("publish: n=%d err=%v", n, err)
	}
	var pending int
	_ = admin.QueryRow(ctx, `SELECT COUNT(*) FROM anomaly_outbox WHERE published_at IS NULL`).Scan(&pending)
	if pending != 0 {
		t.Fatalf("rows still pending: %d", pending)
	}
	tenants := map[string]bool{}
	for _, p := range pub.got {
		if p.EventType != domain.EventAnomalyDetected || p.EventID == "" {
			t.Fatalf("published params: %+v", p)
		}
		tenants[p.TenantID] = true
	}
	if !tenants["tenant-a"] || !tenants["tenant-b"] {
		t.Fatalf("relay must drain every tenant: %v", tenants)
	}

	// Without the relay flag and without a tenant, the app role sees nothing.
	var visible int
	if err := app.QueryRow(ctx, `SELECT COUNT(*) FROM anomaly_outbox`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("unscoped app-role read of outbox: %d err=%v", visible, err)
	}
}
