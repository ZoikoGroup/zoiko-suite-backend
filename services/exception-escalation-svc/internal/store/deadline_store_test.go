package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/store"
)

func TestPgStore_CreateDeadline_LandsScheduled(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	due := time.Now().UTC().Add(72 * time.Hour)
	d, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "Close monthly books",
		LinkedObjectType: "CLOSE_PERIOD", LinkedObjectID: "2026-09", DueAt: due,
		OwnerPrincipalID: "owner-1", CalcRule: "5 business days after period end", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.DeadlineStatusScheduled, d.Status)
	require.Equal(t, "", d.SourceType, "an own deadline must have no source lock")
	require.WithinDuration(t, due, d.DueAt, time.Second)

	got, err := s.GetDeadline(ctx(), d.DeadlineID)
	require.NoError(t, err)
	require.Equal(t, d.DeadlineID, got.DeadlineID)
}

func TestPgStore_GetDeadline_UnknownDeadline_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	_, err := s.GetDeadline(ctx(), "deadline-does-not-exist")
	require.ErrorIs(t, err, domain.ErrDeadlineNotFound)
}

func TestPgStore_MirrorAuthoritativeDeadline_RequiresSourceFields(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	_, _, err := s.MirrorAuthoritativeDeadline(ctx(), domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "GST return", DueAt: time.Now().UTC().Add(24 * time.Hour),
		CreatedByPrincipalID: "creator-1",
	})
	require.ErrorIs(t, err, domain.ErrDeadlineSourceRequired)
}

func TestPgStore_MirrorAuthoritativeDeadline_ThenGetSourceDeadline(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	due := time.Now().UTC().Add(48 * time.Hour)
	d, superseded, err := s.MirrorAuthoritativeDeadline(ctx(), domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "GST return due",
		DueAt: due, OwnerPrincipalID: "owner-1",
		SourceType: "TAX", SourceRef: "gst-filing-123", SourceVersion: "v1", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	require.Equal(t, "", superseded)
	require.Equal(t, domain.DeadlineStatusScheduled, d.Status)
	require.Equal(t, "TAX", d.SourceType)

	info, err := s.GetSourceDeadline(ctx(), "default", d.DeadlineID)
	require.NoError(t, err)
	require.True(t, info.Mirrored)
	require.Equal(t, "gst-filing-123", info.SourceRef)
	require.Equal(t, "v1", info.SourceVersion)
}

// TestPgStore_MirrorAuthoritativeDeadline_ReMirrorSupersedesUntouchedRow
// proves the real DeadlineSuperseded mechanism: re-mirroring a source
// whose local copy is still SCHEDULED (untouched) creates a new row and
// marks the prior one SUPERSEDED, rather than mutating due_at in place.
func TestPgStore_MirrorAuthoritativeDeadline_ReMirrorSupersedesUntouchedRow(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	due1 := time.Now().UTC().Add(48 * time.Hour)
	first, _, err := s.MirrorAuthoritativeDeadline(ctx(), domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "GST return due",
		DueAt: due1, SourceType: "TAX", SourceRef: "gst-filing-999", SourceVersion: "v1", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	due2 := due1.Add(24 * time.Hour)
	second, superseded, err := s.MirrorAuthoritativeDeadline(ctx(), domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "GST return due",
		DueAt: due2, SourceType: "TAX", SourceRef: "gst-filing-999", SourceVersion: "v2", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	require.Equal(t, first.DeadlineID, superseded)
	require.NotEqual(t, first.DeadlineID, second.DeadlineID)
	require.WithinDuration(t, due2, second.DueAt, time.Second)

	gotFirst, err := s.GetDeadline(ctx(), first.DeadlineID)
	require.NoError(t, err)
	require.Equal(t, domain.DeadlineStatusSuperseded, gotFirst.Status)
	require.Equal(t, second.DeadlineID, gotFirst.SupersededByDeadlineID)

	gotSecond, err := s.GetDeadline(ctx(), second.DeadlineID)
	require.NoError(t, err)
	require.Equal(t, domain.DeadlineStatusScheduled, gotSecond.Status)
}

// TestPgStore_MirrorAuthoritativeDeadline_ConflictsWithLocallyCompletedRow
// is the negative control for DEADLINE_SOURCE_CONFLICT: once a human has
// completed the local mirror, a later re-mirror with a changed source
// version must refuse rather than silently rewriting what was already
// acted on.
func TestPgStore_MirrorAuthoritativeDeadline_ConflictsWithLocallyCompletedRow(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	due := time.Now().UTC().Add(48 * time.Hour)
	first, _, err := s.MirrorAuthoritativeDeadline(ctx(), domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "GST return due",
		DueAt: due, SourceType: "TAX", SourceRef: "gst-filing-777", SourceVersion: "v1", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	_, err = s.CompleteDeadline(ctx(), domain.CompleteDeadlineParams{DeadlineID: first.DeadlineID, TenantID: "default", ActorPrincipalID: "owner-1"})
	require.NoError(t, err)

	_, _, err = s.MirrorAuthoritativeDeadline(ctx(), domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "GST return due",
		DueAt: due.Add(24 * time.Hour), SourceType: "TAX", SourceRef: "gst-filing-777", SourceVersion: "v2", CreatedByPrincipalID: "creator-1",
	})
	require.ErrorIs(t, err, domain.ErrDeadlineSourceConflict)

	// The completed row must be untouched by the refused re-mirror.
	stillCompleted, err := s.GetDeadline(ctx(), first.DeadlineID)
	require.NoError(t, err)
	require.Equal(t, domain.DeadlineStatusCompleted, stillCompleted.Status)
}

func TestPgStore_MirrorAuthoritativeDeadline_SameVersionReturnsExistingRow(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	due := time.Now().UTC().Add(48 * time.Hour)
	first, _, err := s.MirrorAuthoritativeDeadline(ctx(), domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "GST return due",
		DueAt: due, SourceType: "TAX", SourceRef: "gst-filing-555", SourceVersion: "v1", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	again, superseded, err := s.MirrorAuthoritativeDeadline(ctx(), domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "GST return due",
		DueAt: due, SourceType: "TAX", SourceRef: "gst-filing-555", SourceVersion: "v1", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	require.Equal(t, "", superseded)
	require.Equal(t, first.DeadlineID, again.DeadlineID)
}

func TestPgStore_AssignOwner_ThenComplete_ThenRejectsDoubleComplete(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	d, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "Vendor onboarding checklist",
		DueAt: time.Now().UTC().Add(120 * time.Hour), CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	assigned, err := s.AssignOwner(ctx(), domain.AssignOwnerParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "supervisor-1", OwnerPrincipalID: "owner-2"})
	require.NoError(t, err)
	require.Equal(t, "owner-2", assigned.OwnerPrincipalID)

	completed, err := s.CompleteDeadline(ctx(), domain.CompleteDeadlineParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "owner-2"})
	require.NoError(t, err)
	require.Equal(t, domain.DeadlineStatusCompleted, completed.Status)
	require.NotNil(t, completed.CompletedAt)

	_, err = s.CompleteDeadline(ctx(), domain.CompleteDeadlineParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "owner-2"})
	require.ErrorIs(t, err, domain.ErrDeadlineInvalidState)

	// AssignOwner after completion is also refused — SCHEDULED only.
	_, err = s.AssignOwner(ctx(), domain.AssignOwnerParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "supervisor-1", OwnerPrincipalID: "owner-3"})
	require.ErrorIs(t, err, domain.ErrDeadlineInvalidState)
}

// TestPgStore_DeadlineEscalations_AreAppendOnly is the negative control
// on migration 000004's trigger, proven directly against the table since
// Escalate itself is Wave 2 — the evidentiary guarantee must already
// hold from the moment the table exists. Uses a single acquired
// connection with a session-level tenant context (rather than the
// store's own per-transaction LOCAL setting) so the row can be inserted
// and then tampered with under the same RLS-visible tenant.
func TestPgStore_DeadlineEscalations_AreAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	d, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "Vendor onboarding checklist",
		DueAt: time.Now().UTC().Add(120 * time.Hour), CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	conn, err := pool.Acquire(ctx())
	require.NoError(t, err)
	defer conn.Release()

	_, err = conn.Exec(ctx(), `SELECT set_config('app.tenant_id', 'default', false)`)
	require.NoError(t, err)

	_, err = conn.Exec(ctx(), `INSERT INTO deadline_escalations (escalation_id, deadline_id, tenant_id, escalated_to_role, escalated_by_principal_id)
		VALUES ('esc-1', $1, 'default', 'FINANCE_LEAD', 'supervisor-1')`, d.DeadlineID)
	require.NoError(t, err)

	_, err = conn.Exec(ctx(), `UPDATE deadline_escalations SET escalated_to_role = 'CFO' WHERE escalation_id = 'esc-1'`)
	require.Error(t, err, "expected the append-only trigger to reject a direct mutation")
}

func TestPgStore_Recalculate_MovesDueAtAndResetsNotifications(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	due := time.Now().UTC().Add(1 * time.Hour) // within the due-soon window
	d, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "Close monthly books",
		DueAt: due, CalcRule: "original rule", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	// Observe it via ListUpcoming so due_soon_notified_at gets set.
	_, justNotified, err := s.ListUpcoming(ctx(), domain.ListUpcomingParams{TenantID: "default", LegalEntityID: "le-1"})
	require.NoError(t, err)
	require.Contains(t, justNotified, d.DeadlineID)

	newDue := due.Add(200 * time.Hour) // outside the due-soon window
	recalced, err := s.Recalculate(ctx(), domain.RecalculateParams{
		DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "creator-1",
		DueAt: newDue, CalcRule: "revised rule",
	})
	require.NoError(t, err)
	require.WithinDuration(t, newDue, recalced.DueAt, time.Second)
	require.Equal(t, "revised rule", recalced.CalcRule)
	require.Nil(t, recalced.DueSoonNotifiedAt, "notification state must reset after recalculation")
}

// TestPgStore_Recalculate_RefusedOnMirroredDeadline is the negative
// control for the doc's own prohibited anti-pattern: "Recalculating
// legal/tax deadlines in BIZ-08 instead of consuming authoritative
// source."
func TestPgStore_Recalculate_RefusedOnMirroredDeadline(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	d, _, err := s.MirrorAuthoritativeDeadline(ctx(), domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "GST return due",
		DueAt:      time.Now().UTC().Add(48 * time.Hour),
		SourceType: "TAX", SourceRef: "gst-filing-recalc", SourceVersion: "v1", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	_, err = s.Recalculate(ctx(), domain.RecalculateParams{
		DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "creator-1",
		DueAt: time.Now().UTC().Add(96 * time.Hour),
	})
	require.ErrorIs(t, err, domain.ErrCannotRecalculateMirroredDeadline)
}

func TestPgStore_Waive_ThenRejectsSecondWaive(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	d, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "Optional internal review",
		DueAt: time.Now().UTC().Add(24 * time.Hour), CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	waived, err := s.Waive(ctx(), domain.WaiveParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "owner-1", Reason: "no longer applicable"})
	require.NoError(t, err)
	require.Equal(t, domain.DeadlineStatusWaived, waived.Status)
	require.Equal(t, "no longer applicable", waived.WaiverReason)

	_, err = s.Waive(ctx(), domain.WaiveParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "owner-1", Reason: "again"})
	require.ErrorIs(t, err, domain.ErrDeadlineInvalidState)
}

func TestPgStore_CancelDeadline_ThenRejectsCancelOfTerminalDeadline(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	d, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "Draft process improvement",
		DueAt: time.Now().UTC().Add(24 * time.Hour), CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	cancelled, err := s.CancelDeadline(ctx(), domain.CancelDeadlineParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "owner-1", Reason: "duplicate"})
	require.NoError(t, err)
	require.Equal(t, domain.DeadlineStatusCancelled, cancelled.Status)

	_, err = s.CancelDeadline(ctx(), domain.CancelDeadlineParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "owner-1", Reason: "again"})
	require.ErrorIs(t, err, domain.ErrDeadlineInvalidState)
}

func TestPgStore_Escalate_RecordsEvidenceWithoutChangingStatus(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	d, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "Vendor SLA review",
		DueAt: time.Now().UTC().Add(1 * time.Hour), CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	esc, err := s.Escalate(ctx(), domain.EscalateParams{
		DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "owner-1", EscalatedToRole: "FINANCE_LEAD", Reason: "at risk of breach",
	})
	require.NoError(t, err)
	require.Equal(t, "FINANCE_LEAD", esc.EscalatedToRole)

	// The doc's own lifecycle line lists no "Escalated" status — the
	// deadline itself must remain SCHEDULED.
	got, err := s.GetDeadline(ctx(), d.DeadlineID)
	require.NoError(t, err)
	require.Equal(t, domain.DeadlineStatusScheduled, got.Status)
}

func TestPgStore_Escalate_RefusedOnTerminalDeadline(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	d, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "Vendor SLA review",
		DueAt: time.Now().UTC().Add(1 * time.Hour), CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	_, err = s.CompleteDeadline(ctx(), domain.CompleteDeadlineParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "owner-1"})
	require.NoError(t, err)

	_, err = s.Escalate(ctx(), domain.EscalateParams{
		DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "owner-1", EscalatedToRole: "FINANCE_LEAD", Reason: "too late",
	})
	require.ErrorIs(t, err, domain.ErrDeadlineInvalidState)
}

// TestPgStore_ListUpcoming_NotifiesOnceThenNotAgain proves the
// first-observation mechanism: the same still-upcoming deadline is only
// ever reported in justNotifiedIDs on the call that first observes it.
func TestPgStore_ListUpcoming_NotifiesOnceThenNotAgain(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	d, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "Close monthly books",
		DueAt: time.Now().UTC().Add(2 * time.Hour), CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	list1, justNotified1, err := s.ListUpcoming(ctx(), domain.ListUpcomingParams{TenantID: "default", LegalEntityID: "le-1"})
	require.NoError(t, err)
	require.Len(t, list1, 1)
	require.Contains(t, justNotified1, d.DeadlineID)

	list2, justNotified2, err := s.ListUpcoming(ctx(), domain.ListUpcomingParams{TenantID: "default", LegalEntityID: "le-1"})
	require.NoError(t, err)
	require.Len(t, list2, 1, "the deadline must still be reported as upcoming")
	require.NotContains(t, justNotified2, d.DeadlineID, "must not be re-notified on a second observation")
}

// TestPgStore_ListOverdue_NotifiesOnceThenNotAgain is ListUpcoming's own
// negative-controlled sibling for overdue_notified_at.
func TestPgStore_ListOverdue_NotifiesOnceThenNotAgain(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	d, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "Expired filing checklist",
		DueAt: time.Now().UTC().Add(-1 * time.Hour), CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	list1, justNotified1, err := s.ListOverdue(ctx(), domain.ListOverdueParams{TenantID: "default", LegalEntityID: "le-1"})
	require.NoError(t, err)
	require.Len(t, list1, 1)
	require.Contains(t, justNotified1, d.DeadlineID)

	list2, justNotified2, err := s.ListOverdue(ctx(), domain.ListOverdueParams{TenantID: "default", LegalEntityID: "le-1"})
	require.NoError(t, err)
	require.Len(t, list2, 1)
	require.NotContains(t, justNotified2, d.DeadlineID)
}

func TestPgStore_ExplainCalculation_OwnVsMirrored(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	own, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "Close monthly books",
		DueAt: time.Now().UTC().Add(24 * time.Hour), CalcRule: "5 business days after period end", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	ownExplain, err := s.ExplainCalculation(ctx(), "default", own.DeadlineID)
	require.NoError(t, err)
	require.False(t, ownExplain.Mirrored)
	require.Equal(t, "5 business days after period end", ownExplain.CalcRule)

	mirrored, _, err := s.MirrorAuthoritativeDeadline(ctx(), domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "GST return due",
		DueAt: time.Now().UTC().Add(48 * time.Hour), SourceType: "TAX", SourceRef: "gst-explain-1", SourceVersion: "v1", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	mirroredExplain, err := s.ExplainCalculation(ctx(), "default", mirrored.DeadlineID)
	require.NoError(t, err)
	require.True(t, mirroredExplain.Mirrored)
	require.Empty(t, mirroredExplain.CalcRule, "a mirrored deadline was never calculated locally")
}
