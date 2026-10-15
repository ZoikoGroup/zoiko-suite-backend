package sweeps

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

type fakeSweepSource struct {
	asOf   []string
	moved  int
	err    error
	events []domain.OutboxEvent
}

func (f *fakeSweepSource) SweepDueAllTenants(_ context.Context, asOf string, mk store.LiabilityEventBuilder) (int, error) {
	f.asOf = append(f.asOf, asOf)
	if mk != nil {
		evs, err := mk(store.LiabilityChange{Kind: "DUE", Liability: &domain.PayrollLiability{LiabilityID: "l-1", TenantID: "t-1", ResultID: "r-1", LegalEntityID: "le-1"}})
		if err != nil {
			return 0, err
		}
		f.events = evs
	}
	return f.moved, f.err
}

func TestRunOnceSweepsAsOfTodayUTCAndBuildsTheDueEvent(t *testing.T) {
	src := &fakeSweepSource{moved: 4}
	d := NewDueSweeper(src, 0, zap.NewNop())
	d.now = func() time.Time { return time.Date(2026, 10, 22, 23, 30, 0, 0, time.FixedZone("x", -5*3600)) }
	assert.Equal(t, 4, d.RunOnce(context.Background()))
	assert.Equal(t, []string{"2026-10-23"}, src.asOf, "the date is taken in UTC")
	require.Len(t, src.events, 1)
	assert.Equal(t, domain.EventPayrollLiabilityDue, src.events[0].EventType)
	assert.Equal(t, "t-1", src.events[0].TenantID)
	assert.Equal(t, time.Hour, d.interval, "default interval")

	src.err = errors.New("db down")
	assert.Equal(t, 4, d.RunOnce(context.Background()), "an error is logged, not fatal")
}

func TestStartRunsImmediatelyAndStopsWithItsContext(t *testing.T) {
	src := &fakeSweepSource{}
	d := NewDueSweeper(src, 10*time.Millisecond, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Start(ctx); close(done) }()
	require.Eventually(t, func() bool { return len(src.asOf) >= 2 }, 2*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not stop")
	}
}
