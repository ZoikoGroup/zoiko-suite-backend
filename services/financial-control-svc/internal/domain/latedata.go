package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Late data after certification (ZS-CONTROL-001 Invariant 14, scenarios 05 and
// 21): "Late-arriving data after population freeze creates an explicit late-data
// exception or a new/superseding run", and the certified population "is not
// silently mutated". The late-data sweep re-reads each source's watermark for
// the latest certified run of a control, entity and period; when it has moved
// past the one the run froze, it opens one LATE_DATA exception on that run. It
// never touches the frozen population, the run or its certification.

// ReasonLateDataAfterCertification is the reason code of that exception. It is
// reserved: no EXCEPTION_SCAN rule may emit it, so the close gate can tell it
// apart from a LATE_DATA finding the certifier already saw.
const ReasonLateDataAfterCertification = "LATE_DATA_AFTER_CERTIFICATION"

// CatLateData is the §21 category of a late-data exception.
const CatLateData = "LATE_DATA"

// noCurrency is ISO 4217 "no currency": the exception records a fact about the
// population, not an amount.
const noCurrency = "XXX"

// LateDataException builds the exception for one side whose source watermark
// moved from snap.Watermark to observed after run was certified. It is born like
// every other exception (Invariant 7): owned by the control's owner role, OPEN,
// with a due date. Severity is HIGH, not derived from exposure: there is no
// amount yet, and the period's certified population no longer describes the
// source. record_ids names the frozen population that went stale.
func LateDataException(run ControlRun, snap PopulationSnapshot, observed, ownerRole string, now time.Time) ControlException {
	return ControlException{
		ExceptionID:   uuid.NewString(),
		TenantID:      run.TenantID,
		RunID:         run.RunID,
		LegalEntityID: run.LegalEntityID,
		Category:      CatLateData,
		ReasonCode:    ReasonLateDataAfterCertification,
		Assertion:     "COMPLETENESS",
		Severity:      SeverityHigh,
		Side:          snap.Side,
		RecordIDs:     []string{snap.PopulationID},
		Exposure:      "0",
		Currency:      noCurrency,
		Detail: fmt.Sprintf("source %s changed after the run was certified: frozen watermark %q, now %q. "+
			"The certified population is unchanged; run the control again naming this run as prior, "+
			"or resolve this exception under authority.", snap.SpecRef, snap.Watermark, observed),
		OwnerRole: ownerRole,
		DueAt:     now.Add(DefaultSLA(SeverityHigh)),
		State:     ExOpen,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
}
