package paymentinit_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/payment-status-svc/internal/domain"
	"zoiko.io/payment-status-svc/internal/paymentinit"
)

type fakeRow struct {
	tenantID, sourceReference string
	status                    domain.ExecutionStatus
}

type fakeStore struct {
	mu    sync.Mutex
	rows  map[string]*fakeRow // tenantID + "\x00" + sourceReference
	calls int
	err   error
}

func newFakeStore(rows ...*fakeRow) *fakeStore {
	m := map[string]*fakeRow{}
	for _, r := range rows {
		m[r.tenantID+"\x00"+r.sourceReference] = r
	}
	return &fakeStore{rows: m}
}

// setErr and setRow let a test mutate the fake concurrently with Run's
// goroutine, matching a real store's own safety (its calls never race).
func (f *fakeStore) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeStore) setRow(r *fakeRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[r.tenantID+"\x00"+r.sourceReference] = r
}

func (f *fakeStore) ApplySubmissionEvent(_ context.Context, tenantID, sourceReference string, toStatus domain.ExecutionStatus, _, _, _ string) (*domain.PaymentExecutionState, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, false, f.err
	}
	row, ok := f.rows[tenantID+"\x00"+sourceReference]
	if !ok {
		return nil, false, domain.ErrUnresolvedPaymentNotFound
	}
	from := map[domain.ExecutionStatus]bool{}
	switch toStatus {
	case domain.StatusSubmitted:
		from[domain.StatusPrepared], from[domain.StatusPendingUnknown] = true, true
	case domain.StatusPendingUnknown:
		from[domain.StatusPrepared], from[domain.StatusSubmitted] = true, true
	}
	if !from[row.status] {
		return &domain.PaymentExecutionState{Status: row.status}, false, nil
	}
	row.status = toStatus
	return &domain.PaymentExecutionState{Status: row.status}, true, nil
}

func event(t *testing.T, id, typ, tenantID string, payload map[string]any) []byte {
	t.Helper()
	raw, _ := json.Marshal(payload)
	b, _ := json.Marshal(map[string]any{"event_id": id, "event_type": typ, "tenant_id": tenantID, "legal_entity_id": "le-1", "payload": json.RawMessage(raw)})
	return b
}

func TestHandle_PaymentSubmitted_AppliesFromPrepared(t *testing.T) {
	store := newFakeStore(&fakeRow{tenantID: "t1", sourceReference: "src-1", status: domain.StatusPrepared})
	h := paymentinit.NewHandler(store, paymentinit.IsTransient, zap.NewNop())
	out, err := h.Handle(context.Background(), event(t, "ev-1", "PAYMENT_SUBMITTED", "t1", map[string]any{"source_reference": "src-1", "provider_ref": "prov-1"}))
	if err != nil || out != paymentinit.OutcomeSubmitted {
		t.Fatalf("outcome = %s %v", out, err)
	}
	if store.rows["t1\x00src-1"].status != domain.StatusSubmitted {
		t.Fatalf("status = %s", store.rows["t1\x00src-1"].status)
	}
}

func TestHandle_PaymentSubmissionAmbiguous_AppliesPendingUnknown(t *testing.T) {
	store := newFakeStore(&fakeRow{tenantID: "t1", sourceReference: "src-1", status: domain.StatusSubmitted})
	h := paymentinit.NewHandler(store, paymentinit.IsTransient, zap.NewNop())
	out, err := h.Handle(context.Background(), event(t, "ev-1", "PAYMENT_SUBMISSION_AMBIGUOUS", "t1", map[string]any{"source_reference": "src-1"}))
	if err != nil || out != paymentinit.OutcomePendingUnknown {
		t.Fatalf("outcome = %s %v", out, err)
	}
	if store.rows["t1\x00src-1"].status != domain.StatusPendingUnknown {
		t.Fatalf("status = %s", store.rows["t1\x00src-1"].status)
	}
}

// Redelivery of an already-applied event is a no-op, not an error.
func TestHandle_RedeliveredSubmitted_IsNoop(t *testing.T) {
	store := newFakeStore(&fakeRow{tenantID: "t1", sourceReference: "src-1", status: domain.StatusSubmitted})
	h := paymentinit.NewHandler(store, paymentinit.IsTransient, zap.NewNop())
	out, err := h.Handle(context.Background(), event(t, "ev-1", "PAYMENT_SUBMITTED", "t1", map[string]any{"source_reference": "src-1"}))
	if err != nil || out != paymentinit.OutcomeNoop {
		t.Fatalf("outcome = %s %v", out, err)
	}
}

// Real provider evidence (ACCEPTED, via a callback) already superseded
// BNK-06's own observation of its submission attempt; a late PAYMENT_SUBMITTED
// must never regress it.
func TestHandle_StaleSubmittedAfterRealProgress_IsNoop(t *testing.T) {
	store := newFakeStore(&fakeRow{tenantID: "t1", sourceReference: "src-1", status: domain.StatusAccepted})
	h := paymentinit.NewHandler(store, paymentinit.IsTransient, zap.NewNop())
	out, err := h.Handle(context.Background(), event(t, "ev-1", "PAYMENT_SUBMITTED", "t1", map[string]any{"source_reference": "src-1"}))
	if err != nil || out != paymentinit.OutcomeNoop {
		t.Fatalf("outcome = %s %v", out, err)
	}
	if store.rows["t1\x00src-1"].status != domain.StatusAccepted {
		t.Fatalf("regressed: %s", store.rows["t1\x00src-1"].status)
	}
}

// PENDING_UNKNOWN -> SUBMITTED: BNK-06 resolving its own ambiguity by
// confirming the attempt did reach the provider after all.
func TestHandle_SubmittedAfterPendingUnknown_Applies(t *testing.T) {
	store := newFakeStore(&fakeRow{tenantID: "t1", sourceReference: "src-1", status: domain.StatusPendingUnknown})
	h := paymentinit.NewHandler(store, paymentinit.IsTransient, zap.NewNop())
	out, err := h.Handle(context.Background(), event(t, "ev-1", "PAYMENT_SUBMITTED", "t1", map[string]any{"source_reference": "src-1"}))
	if err != nil || out != paymentinit.OutcomeSubmitted {
		t.Fatalf("outcome = %s %v", out, err)
	}
}

// The row not existing yet is worth retrying: RecordPaymentStatus for this
// instruction may not have committed yet.
func TestHandle_RowNotFoundYet_IsTransient(t *testing.T) {
	store := newFakeStore()
	h := paymentinit.NewHandler(store, paymentinit.IsTransient, zap.NewNop())
	_, err := h.Handle(context.Background(), event(t, "ev-1", "PAYMENT_SUBMITTED", "t1", map[string]any{"source_reference": "src-404"}))
	if err == nil || !paymentinit.IsTransient(err) {
		t.Fatalf("expected a transient error, got %v", err)
	}
}

func TestHandle_MalformedAndIgnoredEvents(t *testing.T) {
	store := newFakeStore()
	h := paymentinit.NewHandler(store, paymentinit.IsTransient, zap.NewNop())
	if out, _ := h.Handle(context.Background(), []byte("{")); out != paymentinit.OutcomeMalformed {
		t.Errorf("garbage = %s", out)
	}
	if out, _ := h.Handle(context.Background(), event(t, "ev-1", "PAYMENT_SUBMISSION_REJECTED", "t1", map[string]any{"source_reference": "src-1"})); out != paymentinit.OutcomeIgnored {
		t.Errorf("unrelated event type = %s", out)
	}
	if out, _ := h.Handle(context.Background(), event(t, "ev-1", "PAYMENT_SUBMITTED", "t1", map[string]any{})); out != paymentinit.OutcomeMalformed {
		t.Errorf("no source_reference = %s", out)
	}
	if out, _ := h.Handle(context.Background(), event(t, "ev-1", "PAYMENT_SUBMITTED", "", map[string]any{"source_reference": "src-1"})); out != paymentinit.OutcomeMalformed {
		t.Errorf("no tenant_id = %s", out)
	}
	if store.calls != 0 {
		t.Errorf("the store must never be called for a malformed or ignored event, got %d calls", store.calls)
	}
}

// An outage is retried.
func TestHandle_StoreOutage_IsTransient(t *testing.T) {
	store := newFakeStore()
	store.err = domain.ErrStoreUnavailable
	h := paymentinit.NewHandler(store, paymentinit.IsTransient, zap.NewNop())
	_, err := h.Handle(context.Background(), event(t, "ev-1", "PAYMENT_SUBMITTED", "t1", map[string]any{"source_reference": "src-1"}))
	if err == nil || !paymentinit.IsTransient(err) {
		t.Fatalf("expected a transient error, got %v", err)
	}
}

// ── Run loop ─────────────────────────────────────────────────────────────

// fakeReader serves a fixed slice of messages once each, then blocks until ctx
// ends — the same contract kafka.Reader's FetchMessage gives when the topic
// has nothing further.
type fakeReader struct {
	msgs       []kafka.Message
	next       int
	committed  []kafka.Message
	commitErrs int
}

func (f *fakeReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	if f.next < len(f.msgs) {
		m := f.msgs[f.next]
		f.next++
		return m, nil
	}
	<-ctx.Done()
	return kafka.Message{}, ctx.Err()
}

func (f *fakeReader) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	f.committed = append(f.committed, msgs...)
	return nil
}

func (f *fakeReader) Close() error { return nil }

// A message whose source_reference never resolves is retried up to the
// bounded attempt count, then committed (not re-delivered forever) and
// counted as "dropped" — the poison-message / stuck-forever failure mode
// this pattern exists to prevent.
func TestRun_PoisonMessageIsEventuallyDroppedAndCommitted(t *testing.T) {
	store := newFakeStore() // no rows: every attempt returns ErrUnresolvedPaymentNotFound
	h := paymentinit.NewHandler(store, paymentinit.IsTransient, zap.NewNop())
	msg := kafka.Message{Offset: 1, Value: event(t, "ev-1", "PAYMENT_SUBMITTED", "t1", map[string]any{"source_reference": "src-never"})}
	r := &fakeReader{msgs: []kafka.Message{msg}}

	// Run's bounded retry schedule sleeps after every one of the 8 attempts
	// (including the last, before it gives up) at 500ms doubling up to a
	// 32s-ish ceiling: 500ms+1s+2s+4s+8s+16s+32s+32s ≈ 95.5s. This is the
	// real, deliberately slow production behavior (the whole point of the
	// backoff), not something to shorten for the test, so this one needs a
	// generous budget.
	outcomes := make(chan string, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		paymentinit.Run(ctx, r, h, zap.NewNop(), func(o string) { outcomes <- o })
		close(done)
	}()

	select {
	case o := <-outcomes:
		if o != "dropped" {
			t.Fatalf("outcome = %s, want dropped", o)
		}
	case <-time.After(140 * time.Second):
		t.Fatal("timed out waiting for the poison message to be dropped")
	}
	if len(r.committed) != 1 || r.committed[0].Offset != 1 {
		t.Fatalf("committed = %+v; the message must still be committed so it is not redelivered forever", r.committed)
	}
	cancel()
	<-done
}

// A transient failure that clears on retry (an outage recovering) is applied
// on redelivery, without ever being committed-and-dropped.
func TestRun_TransientFailureRecoversWithoutDropping(t *testing.T) {
	store := newFakeStore()
	store.err = domain.ErrStoreUnavailable
	h := paymentinit.NewHandler(store, paymentinit.IsTransient, zap.NewNop())
	msg := kafka.Message{Offset: 1, Value: event(t, "ev-1", "PAYMENT_SUBMITTED", "t1", map[string]any{"source_reference": "src-1"})}
	r := &fakeReader{msgs: []kafka.Message{msg}}

	outcomes := make(chan string, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		paymentinit.Run(ctx, r, h, zap.NewNop(), func(o string) { outcomes <- o })
		close(done)
	}()
	// Clear the outage shortly after the first attempt, well before the
	// bounded retry budget is spent.
	time.Sleep(600 * time.Millisecond)
	store.setErr(nil)
	store.setRow(&fakeRow{tenantID: "t1", sourceReference: "src-1", status: domain.StatusPrepared})

	select {
	case o := <-outcomes:
		if o != paymentinit.OutcomeSubmitted {
			t.Fatalf("outcome = %s, want %s", o, paymentinit.OutcomeSubmitted)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the retried message to apply")
	}
	cancel()
	<-done
}
