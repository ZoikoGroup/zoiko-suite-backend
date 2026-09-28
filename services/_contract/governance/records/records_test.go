package records_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/contract/governance/records"
	"zoiko.io/contract/types"
)

func TestRecordsScenarios(t *testing.T) {
	resolver := records.NewResolver()
	tenantID := types.MustNewV7()
	recordClassID := types.MustNewV7()
	invoiceID := types.MustNewV7()
	now := time.Now().UTC()

	declaration, err := resolver.DeclareRecord(tenantID, recordClassID, "SALES_INVOICE", invoiceID, records.TriggerPosted, "usr_billing", now)
	if err != nil {
		t.Fatalf("failed to declare record: %v", err)
	}

	t.Run("NP-11: Posted journal / declared record edited in place is rejected", func(t *testing.T) {
		err := resolver.AssertRecordImmutability(declaration, true)
		if !errors.Is(err, records.ErrRecordImmutable) {
			t.Fatalf("expected ErrRecordImmutable, got: %v", err)
		}
	})

	t.Run("NP-12: Retention schedule updated retroactively leaves prior calculation version-pinned", func(t *testing.T) {
		scheduleV1 := &records.RetentionScheduleVersion{
			ScheduleID:           types.MustNewV7(),
			TenantID:             tenantID,
			RecordClassID:        recordClassID,
			JurisdictionCode:     "GB",
			LegalRegulatoryBasis: "HMRC_VAT_ACT_1994_S58",
			Version:              1,
			MinRetentionDays:     2191, // ~6 years
			EffectiveFrom:        now.AddDate(-1, 0, 0),
		}

		// Calculate trigger under V1
		triggerDate := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
		trigger, err := resolver.CalculateRetentionTrigger(declaration, scheduleV1, triggerDate, now)
		if err != nil {
			t.Fatalf("failed to calculate trigger: %v", err)
		}

		if trigger.ScheduleVersion != 1 {
			t.Fatalf("expected trigger pinned to schedule version 1, got %d", trigger.ScheduleVersion)
		}
		expectedEligibleDate := triggerDate.AddDate(0, 0, 2191)
		if trigger.EligibleDispositionDate != expectedEligibleDate {
			t.Fatalf("expected eligible date %v, got %v", expectedEligibleDate, trigger.EligibleDispositionDate)
		}

		// Update schedule to version 2 (e.g. extending to 7 years = 2556 days)
		scheduleV2 := resolver.UpdateScheduleVersion(scheduleV1, 2556, now)
		if scheduleV2.Version != 2 {
			t.Fatalf("expected version 2, got %d", scheduleV2.Version)
		}

		// Prior record trigger remains pinned to V1 (NP-12)
		if trigger.ScheduleVersion != 1 || trigger.EligibleDispositionDate != expectedEligibleDate {
			t.Fatalf("prior trigger was erroneously mutated by schedule update")
		}

		// Status before eligible date -> PENDING
		status := resolver.EvaluateEligibility(trigger, triggerDate.AddDate(5, 0, 0))
		if status != records.TriggerStatusPending {
			t.Fatalf("expected status PENDING before eligibility date, got: %s", status)
		}

		// Status after eligible date -> ELIGIBILITY_REACHED (DG-035)
		status = resolver.EvaluateEligibility(trigger, triggerDate.AddDate(7, 0, 0))
		if status != records.TriggerStatusEligibilityReached {
			t.Fatalf("expected status ELIGIBILITY_REACHED after eligibility date, got: %s", status)
		}
	})
}
