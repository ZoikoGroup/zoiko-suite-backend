package bankingrefresh

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/paymentstatus"
	"zoiko.io/payroll-run-svc/internal/store"
)

type fakeAttempts struct {
	status string
	err    error
	asked  []string
}

func (f *fakeAttempts) AttemptStatus(_ context.Context, _, principal, _, attemptID string) (string, error) {
	f.asked = append(f.asked, principal+":"+attemptID)
	return f.status, f.err
}

type fakePayments struct {
	snap *paymentstatus.Snapshot
	err  error
}

func (f *fakePayments) Snapshot(context.Context, string, string, string) (*paymentstatus.Snapshot, error) {
	return f.snap, f.err
}

func instr(attempt, payment *string) domain.PaymentInstruction {
	return domain.PaymentInstruction{InstructionID: "i-1", TenantID: "t-1", LegalEntityID: "le-1", State: domain.PayHandedOff, AttemptID: attempt, PaymentID: payment}
}

func sp(s string) *string { return &s }

func newRefresher(a Attempts, p Payments) *Refresher {
	return New(nil, a, p, "svc", time.Second, 5, zap.NewNop())
}

func TestObserveUsesTheAttemptStatusAndTheServicePrincipal(t *testing.T) {
	a := &fakeAttempts{status: "SUBMITTED"}
	o := newRefresher(a, &fakePayments{}).Observe(context.Background(), instr(sp("att-1"), nil))
	require.NotNil(t, o)
	assert.Equal(t, domain.ObsSourceAttempt, o.Source)
	assert.Equal(t, "SUBMITTED", o.RawStatus)
	assert.Equal(t, domain.OutcomeInFlight, o.Outcome)
	assert.Equal(t, []string{"svc:att-1"}, a.asked)

	a.status = "PENDING_UNKNOWN"
	o = newRefresher(a, &fakePayments{}).Observe(context.Background(), instr(sp("att-1"), nil))
	assert.Equal(t, domain.OutcomeUnresolved, o.Outcome, "unknown is recorded as unknown, never as failed or paid")
}

func TestObserveRecordsNothingWhenBankingCannotBeAsked(t *testing.T) {
	assert.Nil(t, newRefresher(&fakeAttempts{err: errors.New("down")}, &fakePayments{}).Observe(context.Background(), instr(sp("att-1"), nil)))
	assert.Nil(t, newRefresher(&fakeAttempts{status: "SUBMITTED"}, &fakePayments{err: domain.ErrBankingUnavailable}).Observe(context.Background(), instr(sp("att-1"), sp("pay-1"))),
		"an unreachable payment service is unknown: the attempt status alone must not stand in for it")
}

func TestObserveWithoutAnAttemptIsUnresolved(t *testing.T) {
	a := &fakeAttempts{}
	o := newRefresher(a, &fakePayments{}).Observe(context.Background(), instr(nil, nil))
	require.NotNil(t, o)
	assert.Equal(t, domain.ObsSourceHandoff, o.Source)
	assert.Equal(t, domain.OutcomeUnresolved, o.Outcome)
	assert.Empty(t, a.asked, "there is no attempt to ask about")
}

func TestObserveTrustsALinkedPaymentOverTheAttemptButOnlyForTheSameInstruction(t *testing.T) {
	att := &fakeAttempts{status: "SUBMITTED"}
	in := instr(sp("att-1"), sp("pay-1"))
	good := &paymentstatus.Snapshot{PaymentID: "pay-1", LegalEntityID: "le-1", SourceReference: "i-1", Status: "SETTLED"}
	o := newRefresher(att, &fakePayments{snap: good}).Observe(context.Background(), in)
	assert.Equal(t, domain.ObsSourcePayment, o.Source)
	assert.Equal(t, domain.OutcomeSettled, o.Outcome)

	ret := *good
	ret.Status = "RETURNED"
	assert.Equal(t, domain.OutcomeReturned, newRefresher(att, &fakePayments{snap: &ret}).Observe(context.Background(), in).Outcome)

	other := *good
	other.SourceReference = "another-instruction"
	o = newRefresher(att, &fakePayments{snap: &other}).Observe(context.Background(), in)
	assert.Equal(t, domain.OutcomeUnresolved, o.Outcome, "a payment that is not this instruction is never evidence")
	require.NotNil(t, o.Detail)

	wrongEntity := *good
	wrongEntity.LegalEntityID = "le-2"
	assert.Equal(t, domain.OutcomeUnresolved, newRefresher(att, &fakePayments{snap: &wrongEntity}).Observe(context.Background(), in).Outcome)

	conflict := *good
	conflict.HasOpenConflict = true
	assert.Equal(t, domain.OutcomeUnresolved, newRefresher(att, &fakePayments{snap: &conflict}).Observe(context.Background(), in).Outcome,
		"a settled payment with an open conflict is not settled")
}

type fakeSource struct {
	mu      sync.Mutex
	limit   int
	called  int
	returns int
	err     error
}

func (f *fakeSource) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.called
}

func (f *fakeSource) lastLimit() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.limit
}

func (f *fakeSource) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeSource) RefreshBankingOutcomes(_ context.Context, limit int, _ store.ObserveFunc, mk store.ReconEventBuilder) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	f.limit = limit
	if mk == nil {
		return 0, errors.New("no event builder")
	}
	return f.returns, f.err
}

func TestRunOnceUsesTheConfiguredBatchAndSurvivesErrors(t *testing.T) {
	src := &fakeSource{returns: 3}
	r := New(src, &fakeAttempts{}, &fakePayments{}, "", 0, 0, zap.NewNop())
	assert.Equal(t, 3, r.RunOnce(context.Background()))
	assert.Equal(t, 10, src.lastLimit(), "default batch")
	assert.Equal(t, 30*time.Second, r.interval, "default interval")
	assert.Equal(t, "payroll-run-svc", r.principal, "default principal")

	src.setErr(errors.New("db down"))
	assert.Equal(t, 3, r.RunOnce(context.Background()), "an error is logged, not fatal")
	assert.Equal(t, 2, src.calls())
}

func TestStartStopsWithItsContext(t *testing.T) {
	src := &fakeSource{}
	r := New(src, &fakeAttempts{}, &fakePayments{}, "svc", 10*time.Millisecond, 1, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Start(ctx); close(done) }()
	require.Eventually(t, func() bool { return src.calls() >= 2 }, 2*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not stop")
	}
}
