package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func ptr[T any](v T) *T { return &v }

func TestDisposition_AnActiveLegalHoldAlwaysBlocksEvenWhenRetentionIsOver(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(-20, 0, 0)
	d := domain.DecideDisposition(domain.RecordClassPayrollResult, true, ptr("hold-1"), ptr("pol-1"), ptr(2555), &old, now)
	assert.False(t, d.Allowed)
	assert.Equal(t, []string{domain.DispositionHoldActive}, d.Reasons)
	assert.Equal(t, "hold-1", *d.LegalHoldID)
}

func TestDisposition_MissingFactsMeanNotAllowed(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(-20, 0, 0)
	d := domain.DecideDisposition(domain.RecordClassPayrollResult, false, nil, nil, nil, &old, now)
	assert.False(t, d.Allowed, "no retention policy is not permission")
	assert.Contains(t, d.Reasons, domain.DispositionNoPolicy)

	d = domain.DecideDisposition(domain.RecordClassPayrollResult, false, nil, ptr("pol-1"), ptr(2555), nil, now)
	assert.False(t, d.Allowed, "the age of the record is needed to judge retention")
	assert.Contains(t, d.Reasons, domain.DispositionRetentionNotKnown)
}

func TestDisposition_RetentionMustHaveRunOutExactly(t *testing.T) {
	created := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	earliest := created.AddDate(0, 0, 365)
	before := domain.DecideDisposition(domain.RecordClassLiability, false, nil, ptr("p"), ptr(365), &created, earliest.Add(-time.Second))
	assert.False(t, before.Allowed)
	assert.Equal(t, []string{domain.DispositionRetentionNotOver}, before.Reasons)
	assert.Equal(t, earliest, *before.EarliestDisposal)

	at := domain.DecideDisposition(domain.RecordClassLiability, false, nil, ptr("p"), ptr(365), &created, earliest)
	assert.True(t, at.Allowed, "no hold, a policy, and the minimum retention has elapsed")
	assert.Empty(t, at.Reasons)
	assert.Contains(t, at.Note, "authorizes nothing")
}

func TestDisposition_ReasonsAccumulate(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	recent := now.AddDate(0, -1, 0)
	d := domain.DecideDisposition(domain.RecordClassPaymentInstr, true, ptr("h"), ptr("p"), ptr(2555), &recent, now)
	assert.ElementsMatch(t, []string{domain.DispositionHoldActive, domain.DispositionRetentionNotOver}, d.Reasons)
}

func TestDisposition_OnlyWFPClassesAreAccepted(t *testing.T) {
	for _, c := range []string{"WFP_PAYROLL_INPUT", "WFP_PAYROLL_RESULT", "WFP_ACCOUNTING_EVENT", "WFP_LIABILITY", "WFP_PAYMENT_INSTRUCTION", "WFP_RECONCILIATION_EVIDENCE"} {
		assert.True(t, domain.ValidRecordClasses[c], c)
	}
	assert.False(t, domain.ValidRecordClasses["AUDIT_LOG"])
	assert.False(t, domain.ValidRecordClasses[""])
}
