package resolver

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// emptySource has no packs: with the gate off a jurisdiction is simply uncovered. The tests below are about the gate.
type emptySource struct{}

func (emptySource) ListEligiblePacks(context.Context, []string, string, string) ([]domain.EligiblePack, error) {
	return nil, nil
}
func (emptySource) FindPackVersion(context.Context, string, string) (*domain.EligiblePack, error) {
	return nil, errors.New("not used")
}
func (emptySource) LoadPack(context.Context, string) (*domain.LoadedPack, error) {
	return nil, errors.New("not used")
}

func gated(t *testing.T, gate SupportGate, now func() time.Time) *Resolver {
	t.Helper()
	r, err := New(emptySource{}, Config{EligibleStatuses: []string{"RELEASED"}, CacheTTL: 30 * time.Second, SupportGate: gate, Now: now})
	require.NoError(t, err)
	return r
}

func TestSupportGate_RefusesUnsupportedJurisdictionsBeforeLookingAtPacks(t *testing.T) {
	r := gated(t, func(_ context.Context, j string) (SupportAnswer, error) {
		return SupportAnswer{Supported: false, Outcome: "NOT_YET_SUPPORTED"}, nil
	}, nil)
	d, err := r.Resolve(context.Background(), Request{Jurisdiction: "GB", RuleDomain: "TAX", RuleCode: "X", At: time.Now()})
	require.NoError(t, err)
	assert.Equal(t, domain.OutcomeUnsupported, d.Outcome)
	assert.Contains(t, d.Explanation[len(d.Explanation)-1], "NOT_YET_SUPPORTED")

	// The gate covers every resolver entry point, not only Resolve.
	due, err := r.CalculateDue(context.Background(), DueRequest{Jurisdiction: "GB", ObligationCode: "VAT", Facts: domain.DueFacts{PeriodEnd: "2026-06-30"}})
	require.NoError(t, err)
	assert.Equal(t, domain.DueUnsupported, due.Outcome)
	ps, err := r.ResolveParameterSet(context.Background(), ParameterSetRequest{Jurisdiction: "GB", At: time.Now()})
	require.NoError(t, err)
	assert.Equal(t, domain.OutcomeUnsupported, ps.Outcome)
}

func TestSupportGate_FailsClosedWhenTheGateCannotAnswer(t *testing.T) {
	boom := errors.New("rollout store down")
	r := gated(t, func(context.Context, string) (SupportAnswer, error) { return SupportAnswer{}, boom }, nil)
	_, err := r.Resolve(context.Background(), Request{Jurisdiction: "GB", RuleDomain: "TAX", RuleCode: "X", At: time.Now()})
	assert.ErrorIs(t, err, boom, "no answer rather than an ungated one")
}

func TestSupportGate_AnswersAreCachedForTheTTLAndInvalidateClearsThem(t *testing.T) {
	var calls int32
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	r := gated(t, func(context.Context, string) (SupportAnswer, error) {
		atomic.AddInt32(&calls, 1)
		return SupportAnswer{Supported: false, Outcome: "SUSPENDED"}, nil
	}, func() time.Time { return now })
	resolve := func() {
		_, err := r.Resolve(context.Background(), Request{Jurisdiction: "GB", RuleDomain: "TAX", RuleCode: "X", At: now})
		require.NoError(t, err)
	}
	resolve()
	resolve()
	assert.EqualValues(t, 1, atomic.LoadInt32(&calls), "a second call within the TTL is served from the cache")
	now = now.Add(31 * time.Second)
	resolve()
	assert.EqualValues(t, 2, atomic.LoadInt32(&calls), "after the TTL it asks again, so a suspension takes effect within the TTL")
	r.Invalidate()
	resolve()
	assert.EqualValues(t, 3, atomic.LoadInt32(&calls), "Invalidate drops the cached answer at once")
}

func TestSupportGate_OffByDefault(t *testing.T) {
	r := gated(t, nil, nil)
	d, err := r.Resolve(context.Background(), Request{Jurisdiction: "GB", RuleDomain: "TAX", RuleCode: "X", At: time.Now()})
	require.NoError(t, err)
	assert.Equal(t, domain.OutcomeUnsupported, d.Outcome, "no pack covers it, as before")
	assert.NotContains(t, d.Explanation[len(d.Explanation)-1], "rollout", "the gate was not consulted")
}
