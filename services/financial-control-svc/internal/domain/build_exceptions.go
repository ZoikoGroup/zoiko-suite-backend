package domain

import (
	"time"

	"github.com/google/uuid"
)

// BuildExceptions turns the matcher's findings into persistable control
// exceptions. Each one is born with everything Invariant 7 demands:
//
//   - an owner (the control definition's owner ROLE; a named principal is set
//     by the assign command),
//   - a severity DERIVED from the run's pinned materiality (never operator input),
//   - a due date from the default SLA for that severity,
//   - state OPEN, with its finding immutable from here on.
func BuildExceptions(run ControlRun, found []FoundException, mat *MaterialityView, ownerRole string, now time.Time) []ControlException {
	out := make([]ControlException, 0, len(found))
	for _, f := range found {
		sev := DeriveSeverity(f.Category, f.Exposure, f.Currency, mat)
		due := now.Add(DefaultSLA(sev))
		var clearing *string
		if f.ExpectedClearing != nil {
			// A reconciling item is due when it is expected to clear (never before now).
			c := f.ExpectedClearing.Format("2006-01-02")
			clearing = &c
			due = f.ExpectedClearing.Add(24*time.Hour - time.Second)
			if due.Before(now) {
				due = now.Add(DefaultSLA(sev))
			}
		}
		out = append(out, ControlException{
			ExceptionID:      uuid.NewString(),
			TenantID:         run.TenantID,
			RunID:            run.RunID,
			LegalEntityID:    run.LegalEntityID,
			Category:         f.Category,
			ReasonCode:       f.Reason,
			Assertion:        f.Assertion,
			Severity:         sev,
			Side:             f.Side,
			RecordIDs:        f.RecordIDs,
			Exposure:         f.Exposure,
			Currency:         f.Currency,
			Detail:           f.Detail,
			OwnerRole:        ownerRole,
			DueAt:            due,
			ExpectedClearing: clearing,
			State:            ExOpen,
			Version:          1,
			CreatedAt:        now,
			UpdatedAt:        now,
		})
	}
	return out
}
