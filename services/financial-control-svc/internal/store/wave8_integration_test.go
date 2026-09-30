//go:build integration

package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/catalogue"
	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/store"
)

func TestWave8_RootCause_RequiredForHighSeverityAndRecurrence(t *testing.T) {
	w := newWave5(t)
	// Materiality pins HIGH at 100, so the 250.00 finding is individually material.
	_, err := testStore.CreateMaterialityPolicy(ctx, w.tenant, "maker", domain.CreateMaterialityPolicyRequest{
		LegalEntityID: w.entity, ReportingBasis: "US_GAAP", AmountThreshold: "100", AggregateThreshold: "1000",
		Currency: "USD", EffectiveFrom: "2026-01-01", ApprovedBy: "cfo"})
	require.NoError(t, err)

	_, exc := failedRunWithTwoExceptions(t, w) // e1=250.00 (HIGH), e2=40.00 (LOW)
	var high, low *domain.ControlException
	for i := range exc {
		if exc[i].Severity == domain.SeverityHigh {
			high = &exc[i]
		} else {
			low = &exc[i]
		}
	}
	require.NotNil(t, high)
	require.NotNil(t, low)

	for _, e := range []*domain.ControlException{high, low} {
		_, err := step(t, w, e, "olivia", domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "looking"})
		require.NoError(t, err)
		_, err = step(t, w, e, "olivia", domain.ResolveExceptionRequest{ToState: "AWAITING_ADJUSTMENT", Reason: "journal"})
		require.NoError(t, err)
	}
	_, err = step(t, w, high, "olivia", domain.ResolveExceptionRequest{ToState: "REMEDIATED", Reason: "fixed", EvidenceRef: "JE-1"})
	require.ErrorIs(t, err, domain.ErrInvalidArgument, "a HIGH exception needs a root cause")
	_, err = step(t, w, high, "olivia", domain.ResolveExceptionRequest{ToState: "REMEDIATED", Reason: "fixed", EvidenceRef: "JE-1",
		RootCauseCode: "LATE_SUBLEDGER_FEED", RootCauseNote: "feed landed after cut-off"})
	require.NoError(t, err)
	_, err = step(t, w, low, "olivia", domain.ResolveExceptionRequest{ToState: "REMEDIATED", Reason: "fixed", EvidenceRef: "JE-2"})
	require.NoError(t, err, "a first-time LOW exception needs none")

	hist, err := testStore.ListExceptionTransitions(ctx, w.tenant, high.ExceptionID)
	require.NoError(t, err)
	assert.Equal(t, "LATE_SUBLEDGER_FEED", hist[len(hist)-1].RootCauseCode)

	// The same underlying record failing again in a later run is recurrent: a root cause is now
	// required even though the exception is LOW.
	w.src.set(catalogue.SysGL, "control-account-postings", "wm", r("e2", "J-2", "40.00"))
	run2, _ := w.run("FIN-CTRL-021", fiscal)
	list, err := testStore.ListExceptions(ctx, w.tenant, run2.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	again, err := testStore.AssignException(ctx, w.tenant, list[0].ExceptionID, "lead", "c", list[0].Version,
		domain.AssignExceptionRequest{OwnerPrincipalID: "olivia", Reason: "owner"})
	require.NoError(t, err)
	for _, to := range []string{"INVESTIGATING", "AWAITING_ADJUSTMENT"} {
		_, err = step(t, w, again, "olivia", domain.ResolveExceptionRequest{ToState: to, Reason: "x"})
		require.NoError(t, err)
	}
	require.NotEqual(t, domain.SeverityHigh, again.Severity)
	_, err = step(t, w, again, "olivia", domain.ResolveExceptionRequest{ToState: "REMEDIATED", Reason: "fixed", EvidenceRef: "JE-3"})
	require.ErrorIs(t, err, domain.ErrInvalidArgument, "a recurring exception needs a root cause")
}

func TestWave8_Monitoring_ComputesTheSectionTwentyEightSignals(t *testing.T) {
	w := newWave5(t)
	empty, err := testStore.Monitoring(ctx, w.tenant, w.entity, "2026-09", time.Now().UTC())
	require.NoError(t, err)
	assert.NotNil(t, empty.Rates.ControlCompletion, "definitions exist, so the completion rate is defined")
	assert.Zero(t, *empty.Rates.ControlCompletion)
	assert.Nil(t, empty.Rates.RecurringException, "no exceptions: recurrence is no-data, not 0%")
	assert.Contains(t, empty.Unavailable, "suspense_aging")
	assert.Contains(t, empty.Unavailable, "manual_match_rate")

	_, exc := failedRunWithTwoExceptions(t, w)
	require.Len(t, exc, 2)
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	_, final := w.run("FIN-CTRL-020", fiscal)
	_, err = testStore.CertifyRun(ctx, w.tenant, final.RunID, "checker", "c", final.Version, domain.CertifyRequest{Decision: "CERTIFY"})
	require.NoError(t, err)

	m, err := testStore.Monitoring(ctx, w.tenant, w.entity, "2026-09", time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, 2, m.Completion.WithRun)
	assert.Equal(t, 2, m.Completion.Executed)
	assert.Equal(t, 1, m.Completion.Certified)
	assert.Equal(t, 2, m.Exceptions.Unresolved)
	assert.Equal(t, 2, m.Aging.UpTo7Days)
	assert.Equal(t, 0, m.Exceptions.SLABreached)
	assert.Positive(t, m.CertificationLatency.Samples)

	later, err := testStore.Monitoring(ctx, w.tenant, w.entity, "2026-09", time.Now().UTC().Add(45*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 2, later.Aging.Days31To90, "aging is computed from the supplied clock, so it is reproducible")
	assert.Equal(t, 2, later.Exceptions.SLABreached)

	other, err := testStore.Monitoring(ctx, w.tenant, "00000000-0000-0000-0000-000000000000", "", time.Now().UTC())
	require.NoError(t, err)
	assert.Zero(t, other.Completion.WithRun, "another entity's runs are not counted")
}
