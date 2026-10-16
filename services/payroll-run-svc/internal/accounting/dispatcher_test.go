package accounting_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/accounting"
	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

type fakeSource struct {
	calls   atomic.Int32
	limit   int
	built   []domain.OutboxEvent
	posted  int
	err     error
	posters []domain.AccountingBridge
}

func (f *fakeSource) DispatchPending(ctx context.Context, limit int, post store.PostFunc, mk store.AccountingEventBuilder) (int, error) {
	f.calls.Add(1)
	f.limit = limit
	if f.err != nil {
		return 0, f.err
	}
	b := domain.AccountingBridge{
		BridgeID: "b-1", TenantID: "t-1", ResultID: "res-1", LegalEntityID: "le-1", State: domain.BridgePosted,
		PostingPurpose: domain.PurposeResult, CorrelationID: "corr-1", EvidenceFlags: []string{},
	}
	out := post(ctx, b, "")
	if out.Posted {
		f.posted++
	}
	evs, err := mk(store.AccountingChange{Kind: store.AccChangePosted, Bridge: &b})
	if err != nil {
		return 0, err
	}
	f.built = evs
	return f.posted, nil
}

type fakePoster struct{ called int }

func (p *fakePoster) Post(_ context.Context, b domain.AccountingBridge, original string) store.PostOutcome {
	p.called++
	return store.PostOutcome{Posted: true, ExecutionID: "e", JournalID: "j"}
}

func TestRunOnceUsesTheLedgerClientAndWritesSystemEvents(t *testing.T) {
	src, poster := &fakeSource{}, &fakePoster{}
	d := accounting.New(src, poster, time.Second, 7, zap.NewNop())

	assert.Equal(t, 1, d.RunOnce(context.Background()))
	assert.Equal(t, 7, src.limit, "the batch size bounds how long row locks are held")
	assert.Equal(t, 1, poster.called)
	require.Len(t, src.built, 1)
	assert.Equal(t, domain.EventPayrollAccountingPosted, src.built[0].EventType)

	var env map[string]any
	require.NoError(t, json.Unmarshal(src.built[0].Payload, &env))
	assert.Equal(t, "payroll-run-svc", env["actor_id"], "no human caused the ledger's answer")
	assert.Equal(t, "t-1", env["tenant_id"])
	assert.Equal(t, "corr-1", env["correlation_id"])
}

func TestDefaultsAndErrorsDoNotStopTheDispatcher(t *testing.T) {
	src := &fakeSource{err: assertErr("db down")}
	d := accounting.New(src, &fakePoster{}, 0, 0, zap.NewNop())
	assert.Equal(t, 0, d.RunOnce(context.Background()), "an error is logged and the loop carries on")
	assert.Equal(t, 10, src.limit, "a small default batch")
}

func TestStartStopsWithItsContextAndKeepsRunningBetweenTicks(t *testing.T) {
	src := &fakeSource{}
	d := accounting.New(src, &fakePoster{}, 5*time.Millisecond, 3, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Start(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for src.calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("the dispatcher ran %d times; expected it to keep ticking", src.calls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the dispatcher did not stop when its context ended")
	}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
