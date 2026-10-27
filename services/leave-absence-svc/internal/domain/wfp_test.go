package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/leave-absence-svc/internal/domain"
)

func approvedLeave() domain.SourceLeave {
	by, at := "mgr-1", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	return domain.SourceLeave{
		RequestID: "r-1", WorkerID: "w-1", LeaveTypeID: "t-1", LeaveTypeCode: "VACATION", IsPaid: true,
		StartDate: "2026-09-10", EndDate: "2026-09-11", Units: "16.00", Status: "APPROVED", ApprovedBy: &by, ApprovedAt: &at,
	}
}

func TestClassifyLine(t *testing.T) {
	const ps, pe = "2026-09-01", "2026-09-30"
	assert.Equal(t, "", domain.ClassifyLine(approvedLeave(), ps, pe, ""))

	l := approvedLeave()
	l.Status = "SUBMITTED"
	assert.Equal(t, domain.ReasonNotApproved, domain.ClassifyLine(l, ps, pe, ""), "unapproved source units never enter payroll")

	l = approvedLeave()
	l.ApprovedBy = nil
	assert.Equal(t, domain.ReasonMissingApprovalEvidence, domain.ClassifyLine(l, ps, pe, ""))
	l = approvedLeave()
	blank := "  "
	l.ApprovedBy = &blank
	assert.Equal(t, domain.ReasonMissingApprovalEvidence, domain.ClassifyLine(l, ps, pe, ""))
	l = approvedLeave()
	l.ApprovedAt = nil
	assert.Equal(t, domain.ReasonMissingApprovalEvidence, domain.ClassifyLine(l, ps, pe, ""))

	l = approvedLeave()
	l.StartDate = "2026-08-31"
	assert.Equal(t, domain.ReasonCrossPeriod, domain.ClassifyLine(l, ps, pe, ""), "no invented per-day allocation")
	l = approvedLeave()
	l.EndDate = "2026-10-01"
	assert.Equal(t, domain.ReasonCrossPeriod, domain.ClassifyLine(l, ps, pe, ""))
	l = approvedLeave()
	l.StartDate, l.EndDate = ps, pe
	assert.Equal(t, "", domain.ClassifyLine(l, ps, pe, ""), "period boundaries are inclusive")

	assert.Equal(t, "NO_ACTIVE_PROFILE", domain.ClassifyLine(approvedLeave(), ps, pe, "NO_ACTIVE_PROFILE"))

	// The most fundamental failure is the one recorded.
	l = approvedLeave()
	l.Status, l.StartDate = "CANCELLED", "2026-01-01"
	assert.Equal(t, domain.ReasonNotApproved, domain.ClassifyLine(l, ps, pe, "WORKER_NOT_FOUND"))
}

func TestLineHash_DetectsAnyMaterialChange(t *testing.T) {
	base := approvedLeave()
	h := domain.LineHash(base)
	assert.Len(t, h, 64)
	assert.Equal(t, h, domain.LineHash(approvedLeave()))

	for name, mutate := range map[string]func(*domain.SourceLeave){
		"units":    func(l *domain.SourceLeave) { l.Units = "16.01" },
		"worker":   func(l *domain.SourceLeave) { l.WorkerID = "w-2" },
		"dates":    func(l *domain.SourceLeave) { l.EndDate = "2026-09-12" },
		"paid":     func(l *domain.SourceLeave) { l.IsPaid = false },
		"type":     func(l *domain.SourceLeave) { l.LeaveTypeID = "t-2" },
		"approver": func(l *domain.SourceLeave) { x := "mgr-2"; l.ApprovedBy = &x },
		"approved at": func(l *domain.SourceLeave) {
			x := l.ApprovedAt.Add(time.Second)
			l.ApprovedAt = &x
		},
	} {
		l := approvedLeave()
		mutate(&l)
		assert.NotEqual(t, h, domain.LineHash(l), name)
	}
}

func TestSourceHash_OrderIndependentAndPopulationSensitive(t *testing.T) {
	a := domain.TimeLeaveLine{SourceLineID: "a", LineHash: "h1"}
	b := domain.TimeLeaveLine{SourceLineID: "b", LineHash: "h2"}
	assert.Equal(t, domain.SourceHash([]domain.TimeLeaveLine{a, b}), domain.SourceHash([]domain.TimeLeaveLine{b, a}))
	assert.NotEqual(t, domain.SourceHash([]domain.TimeLeaveLine{a, b}), domain.SourceHash([]domain.TimeLeaveLine{a}))
	changed := b
	changed.LineHash = "h3"
	assert.NotEqual(t, domain.SourceHash([]domain.TimeLeaveLine{a, b}), domain.SourceHash([]domain.TimeLeaveLine{a, changed}))
	assert.Len(t, domain.SourceHash(nil), 64, "an empty population still has a hash")
}

func TestImportRequestValidation(t *testing.T) {
	good := domain.ImportTimeLeaveBatchRequest{LegalEntityID: "le-1", PayrollPeriodID: "2026-09", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30"}
	require.NoError(t, good.Validate())
	assert.Equal(t, domain.OnInvalidRejectBatch, good.WithDefaults().OnInvalid, "the conservative policy is the default")

	for name, mutate := range map[string]func(*domain.ImportTimeLeaveBatchRequest){
		"entity":     func(r *domain.ImportTimeLeaveBatchRequest) { r.LegalEntityID = " " },
		"period id":  func(r *domain.ImportTimeLeaveBatchRequest) { r.PayrollPeriodID = "" },
		"start":      func(r *domain.ImportTimeLeaveBatchRequest) { r.PeriodStart = "01/09/2026" },
		"end":        func(r *domain.ImportTimeLeaveBatchRequest) { r.PeriodEnd = "x" },
		"reversed":   func(r *domain.ImportTimeLeaveBatchRequest) { r.PeriodStart, r.PeriodEnd = "2026-09-30", "2026-09-01" },
		"on_invalid": func(r *domain.ImportTimeLeaveBatchRequest) { r.OnInvalid = "IGNORE" },
	} {
		r := good
		mutate(&r)
		assert.Error(t, r.Validate(), name)
	}
}

func TestEventCatalogue(t *testing.T) {
	for _, e := range []string{
		"time_leave_financial_batch_received", "time_leave_financial_batch_accepted",
		"time_leave_line_quarantined", "time_leave_financial_batch_frozen",
	} {
		assert.True(t, domain.EventCatalogue[e], e)
	}
	assert.False(t, domain.EventCatalogue["leave_paid"])
}
