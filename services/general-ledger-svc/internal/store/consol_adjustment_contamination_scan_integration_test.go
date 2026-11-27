//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/general-ledger-svc/internal/store"
)

// insertShapedJournal writes a journal_headers row directly (bypassing the HTTP surface and posting-rule
// resolution, which is irrelevant to what the scan looks at) so a test can place one with an exact shape and
// created_at. sourceEventID is nil for an ordinary manual journal that never carries one.
func insertShapedJournal(t *testing.T, c *cenv, journalID, entity string, sourceEventID any, at time.Time) {
	t.Helper()
	if err := c.exec(`
		INSERT INTO journal_headers (
			journal_id, tenant_id, legal_entity_id, fiscal_period, status, description,
			created_by_principal_id, correlation_id, created_at, posted_at, posted_by_principal_id,
			source_event_id, journal_type, transaction_date, posting_date, currency_code, approval_status
		) VALUES (
			$1::uuid, $2::uuid, $3::uuid, $4, 'FINALIZED', $5,
			'svc-consolidation', $6, $7::timestamptz, $7::timestamptz, 'svc-consolidation',
			$8, 'ADJUSTMENT', $7::date, $7::date, 'GBP', 'POSTED'
		)`,
		journalID, c.tenant, entity, "2026-03", "test journal",
		uuid.NewString(), at, sourceEventID); err != nil {
		t.Fatal(err)
	}
}

// TestConsolAdjustmentContaminationScan proves the SPEC_DEVIATIONS.md A1/B12 diagnostic (consol-adjustment-
// contamination-scan) against real Postgres: it flags a journal shaped exactly like the OLD, now-replaced
// vulnerable consolidation-svc -> general-ledger-svc posting path would have produced, and does NOT flag (a) an
// unrelated normal journal on an entity that is not any group's reporting entity, (b) an ordinary manual journal
// on the group's reporting entity with no source_event_id, or (c) a journal actually produced by the CURRENT,
// safe group-book path (a real proposed, approved and applied consolidation adjustment).
func TestConsolAdjustmentContaminationScan(t *testing.T) {
	c := consolSetup(t)
	c.seal(t, c.uk, "2026-03")
	c.seal(t, c.us, "2026-03")

	now := time.Now().UTC()
	windowStart, windowEnd := now.Add(-24*time.Hour), now.Add(24*time.Hour)

	// 1. Shaped exactly like the old vulnerable path: no book, a caller-declared source_event_id, on the
	// group's reporting entity, inside the window. This is the one candidate the scan should find.
	contaminatedJournalID := uuid.NewString()
	insertShapedJournal(t, c, contaminatedJournalID, c.group, uuid.NewString(), now)

	// 2. Same shape, but on an entity that is not any group's reporting entity: must not be flagged.
	insertShapedJournal(t, c, uuid.NewString(), c.us, uuid.NewString(), now)

	// 3. On the group's reporting entity, inside the window, but no source_event_id at all (an ordinary manual
	// journal — the generic system-posting path always set one): must not be flagged.
	insertShapedJournal(t, c, uuid.NewString(), c.group, nil, now)

	// 4. A journal actually produced by the CURRENT, safe path: propose, approve and apply a real consolidation
	// adjustment through the real HTTP surface, exactly as consolidation-svc does today. Must not be flagged.
	path := "/v1/consolidation-groups/" + c.groupID + "/adjustments"
	st, adj := c.call("POST", path, c.tenant, "group-finance", adjBody(c.groupID, "safe path adjustment", "40.00"))
	if st != 201 {
		t.Fatalf("propose adjustment: %d %v", st, adj)
	}
	adjID, ok := adj["adjustment_id"].(string)
	if !ok {
		t.Fatalf("propose adjustment: no adjustment_id: %v", adj)
	}
	if st, b := c.call("POST", "/v1/consolidation-adjustments/"+adjID+"/approve", c.tenant, "group-controller", nil); st != 200 {
		t.Fatalf("approve adjustment: %d %v", st, b)
	}
	run := c.mustRun(t, "2026-03")
	if bc, _ := run["blocking_count"].(float64); bc != 0 {
		t.Fatalf("run blocked: %v", run["population"])
	}
	if n := c.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND journal_purpose = 'GROUP_ADJUSTMENT'`, c.tenant, c.group); n != 1 {
		t.Fatalf("expected the safe-path run to post exactly one group-adjustment journal, got %d", n)
	}

	report, err := store.ScanHistoricalGroupBookContamination(context.Background(), c.admin, windowStart, windowEnd)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(report.Candidates) != 1 {
		t.Fatalf("want exactly 1 candidate, got %d: %+v", len(report.Candidates), report.Candidates)
	}
	got := report.Candidates[0]
	if got.JournalID != contaminatedJournalID {
		t.Fatalf("flagged the wrong journal: got %+v, want journal_id=%s", got, contaminatedJournalID)
	}
	if got.LegalEntityID != c.group {
		t.Fatalf("wrong legal entity on the flagged candidate: %+v", got)
	}
	if got.TenantID != c.tenant {
		t.Fatalf("wrong tenant on the flagged candidate: %+v", got)
	}
}

// TestDefaultContaminationWindowMatchesSpecDeviations pins the default scan window to the exact dates
// SPEC_DEVIATIONS.md names for consolidation-svc/general-ledger-svc's A1/B12 (no database needed).
func TestDefaultContaminationWindowMatchesSpecDeviations(t *testing.T) {
	start, end := store.DefaultContaminationWindow()
	if got := start.Format("2006-01-02"); got != "2026-09-07" {
		t.Fatalf("window start = %s, want 2026-09-07", got)
	}
	// Exclusive upper bound: everything through the end of 2026-09-30, nothing on or after 2026-10-01.
	lastMomentOfWindow := time.Date(2026, time.September, 30, 23, 59, 59, 0, time.UTC)
	firstMomentAfter := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	if !lastMomentOfWindow.Before(end) {
		t.Fatalf("window end %s excludes 2026-09-30 23:59:59", end)
	}
	if firstMomentAfter.Before(end) {
		t.Fatalf("window end %s wrongly includes 2026-10-01", end)
	}
}
