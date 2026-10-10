package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/middleware"
	"zoiko.io/document-vault-svc/internal/store"
)

// DRC-03 Retention, Disposition & Legal Hold (ZS-SVC-S-001 §5), dry-run
// only, against real Postgres as the FORCE-RLS app role.

func createTestRecord(t *testing.T, s *store.PgStore, title, recordClass, jurisdiction string) *domain.Record {
	t.Helper()
	doc, v := createTestDocument(t, s, title)
	rec, _, err := s.DeclareRecordV2(tenantCtx(), domain.DeclareRecordV2Params{
		DocumentID: doc.DocumentID, DocumentVersionID: v.DocumentVersionID, LegalEntityID: doc.LegalEntityID,
		RecordClass: recordClass, JurisdictionScope: jurisdiction, DeclaredByPrincipalID: "reviewer-bob",
		DeclarationReason: "test fixture", CorrelationID: "corr-record-" + title,
	})
	require.NoError(t, err)
	return rec
}

func createActiveRetentionRule(t *testing.T, s *store.PgStore, recordClass, jurisdiction string, durationDays int) *domain.RetentionRuleVersion {
	t.Helper()
	rr, err := s.CreateRetentionRuleVersion(tenantCtx(), domain.CreateRetentionRuleVersionParams{
		RecordClass: recordClass, JurisdictionSelector: jurisdiction, LegalBasisRef: "statute-123",
		PurposeRef: "tax-compliance", TriggerType: string(domain.TriggerDeclaredAt), DurationDays: durationDays,
		DispositionAction: string(domain.DispositionDelete), CreatedByPrincipalID: "author-alice",
	})
	require.NoError(t, err)
	approved, err := s.ApproveRetentionRuleVersion(tenantCtx(), domain.ApproveRetentionRuleVersionParams{
		RetentionRuleVersionID: rr.RetentionRuleVersionID, ApprovedByPrincipalID: "approver-carol",
	})
	require.NoError(t, err)
	active, err := s.ActivateRetentionRuleVersion(tenantCtx(), approved.RetentionRuleVersionID)
	require.NoError(t, err)
	return active
}

// Maker-checker: the same principal cannot author and approve a
// retention rule.
func TestDRC03_ApproveRetentionRuleVersion_SelfApprovalRefused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	rr, err := s.CreateRetentionRuleVersion(tenantCtx(), domain.CreateRetentionRuleVersionParams{
		RecordClass: string(domain.RecordClassTaxReturnSupport), JurisdictionSelector: "US-CA",
		TriggerType: string(domain.TriggerDeclaredAt), DurationDays: 2555, DispositionAction: string(domain.DispositionDelete),
		CreatedByPrincipalID: "author-alice",
	})
	require.NoError(t, err)

	_, err = s.ApproveRetentionRuleVersion(tenantCtx(), domain.ApproveRetentionRuleVersionParams{
		RetentionRuleVersionID: rr.RetentionRuleVersionID, ApprovedByPrincipalID: "author-alice",
	})
	require.ErrorIs(t, err, domain.ErrRetentionRuleSelfApproval)
}

// Happy path: a record is bound to an ACTIVE rule matching its own
// class/jurisdiction, with an immediately-known trigger date.
func TestDRC03_BindRetentionRule_HappyPath(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "bind-happy", string(domain.RecordClassTaxReturnSupport), "US-CA")
	rule := createActiveRetentionRule(t, s, string(domain.RecordClassTaxReturnSupport), "US-CA", 30)

	trigger := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st, err := s.BindRetentionRule(tenantCtx(), domain.BindRetentionRuleParams{
		RecordID: rec.RecordID, RetentionRuleVersionID: rule.RetentionRuleVersionID,
		TriggerDate: &trigger, CreatedByPrincipalID: "operator-dave",
	})
	require.NoError(t, err)
	require.Equal(t, domain.RetentionActive, st.State)
	require.NotNil(t, st.DueAt)
	require.Equal(t, time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC), st.DueAt.UTC())
}

// A rule bound to a record with a mismatched class/jurisdiction is
// refused outright.
func TestDRC03_BindRetentionRule_MismatchRefused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "bind-mismatch", string(domain.RecordClassLegalContract), "US-NY")
	rule := createActiveRetentionRule(t, s, string(domain.RecordClassTaxReturnSupport), "US-CA", 30)

	_, err := s.BindRetentionRule(tenantCtx(), domain.BindRetentionRuleParams{
		RecordID: rec.RecordID, RetentionRuleVersionID: rule.RetentionRuleVersionID, CreatedByPrincipalID: "operator-dave",
	})
	require.ErrorIs(t, err, domain.ErrRetentionRuleMismatch)
}

// Binding to a DRAFT (not yet ACTIVE) rule is refused.
func TestDRC03_BindRetentionRule_InactiveRuleRefused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "bind-inactive", string(domain.RecordClassTaxReturnSupport), "US-CA")
	draftRule, err := s.CreateRetentionRuleVersion(tenantCtx(), domain.CreateRetentionRuleVersionParams{
		RecordClass: string(domain.RecordClassTaxReturnSupport), JurisdictionSelector: "US-CA",
		TriggerType: string(domain.TriggerDeclaredAt), DurationDays: 30, DispositionAction: string(domain.DispositionDelete),
		CreatedByPrincipalID: "author-alice",
	})
	require.NoError(t, err)

	_, err = s.BindRetentionRule(tenantCtx(), domain.BindRetentionRuleParams{
		RecordID: rec.RecordID, RetentionRuleVersionID: draftRule.RetentionRuleVersionID, CreatedByPrincipalID: "operator-dave",
	})
	require.ErrorIs(t, err, domain.ErrRetentionRuleNotActive)
}

// Without a trigger date, a binding starts WAITING_FOR_TRIGGER;
// RecordTriggerEvent later moves it to ACTIVE with due_at computed.
func TestDRC03_WaitingForTrigger_ThenRecordTriggerEvent(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "waiting-trigger", string(domain.RecordClassEmploymentRecord), "US-TX")
	rule := createActiveRetentionRule(t, s, string(domain.RecordClassEmploymentRecord), "US-TX", 90)

	st, err := s.BindRetentionRule(tenantCtx(), domain.BindRetentionRuleParams{
		RecordID: rec.RecordID, RetentionRuleVersionID: rule.RetentionRuleVersionID, CreatedByPrincipalID: "operator-dave",
	})
	require.NoError(t, err)
	require.Equal(t, domain.RetentionWaitingForTrigger, st.State)
	require.Nil(t, st.DueAt)

	triggerDate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	triggered, err := s.RecordTriggerEvent(tenantCtx(), domain.RecordTriggerEventParams{
		RetentionStateID: st.RetentionStateID, TriggerDate: triggerDate,
	})
	require.NoError(t, err)
	require.Equal(t, domain.RetentionActive, triggered.State)
	require.Equal(t, time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC), triggered.DueAt.UTC())
}

// EvaluateDue refuses to mark a retention as DUE before its own due_at.
func TestDRC03_EvaluateDue_RefusedBeforeDueDate(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "not-yet-due", string(domain.RecordClassTaxReturnSupport), "US-CA")
	rule := createActiveRetentionRule(t, s, string(domain.RecordClassTaxReturnSupport), "US-CA", 365)

	trigger := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st, err := s.BindRetentionRule(tenantCtx(), domain.BindRetentionRuleParams{
		RecordID: rec.RecordID, RetentionRuleVersionID: rule.RetentionRuleVersionID, TriggerDate: &trigger, CreatedByPrincipalID: "operator-dave",
	})
	require.NoError(t, err)

	_, err = s.EvaluateDue(tenantCtx(), domain.EvaluateDueParams{RetentionStateID: st.RetentionStateID, AsOf: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)})
	require.ErrorIs(t, err, domain.ErrNotYetDue)

	due, err := s.EvaluateDue(tenantCtx(), domain.EvaluateDueParams{RetentionStateID: st.RetentionStateID, AsOf: time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)})
	require.NoError(t, err)
	require.Equal(t, domain.RetentionDue, due.State)
}

// DRC-I10/I12 — the central invariant of this wave: a record under an
// ACTIVE legal hold cannot be approved for disposition, even once its
// retention review is otherwise complete.
func TestDRC03_LegalHold_BlocksApproveForDisposition(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "hold-blocks", string(domain.RecordClassLegalMatterRecord), "US-DE")
	rule := createActiveRetentionRule(t, s, string(domain.RecordClassLegalMatterRecord), "US-DE", 1)

	trigger := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st, err := s.BindRetentionRule(tenantCtx(), domain.BindRetentionRuleParams{
		RecordID: rec.RecordID, RetentionRuleVersionID: rule.RetentionRuleVersionID, TriggerDate: &trigger, CreatedByPrincipalID: "operator-dave",
	})
	require.NoError(t, err)
	st, err = s.EvaluateDue(tenantCtx(), domain.EvaluateDueParams{RetentionStateID: st.RetentionStateID, AsOf: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)})
	require.NoError(t, err)
	st, err = s.FlagForReview(tenantCtx(), domain.FlagForReviewParams{RetentionStateID: st.RetentionStateID, ReviewedByPrincipalID: "reviewer-eve", ReviewNotes: "ready"})
	require.NoError(t, err)
	require.Equal(t, domain.RetentionReviewRequired, st.State)

	hold, err := s.CreateLegalHold(tenantCtx(), domain.CreateLegalHoldParams{
		MatterRef: "Matter-2026-001", HoldReasonCode: "LITIGATION", RecordIDs: []string{rec.RecordID}, IssuedByPrincipalID: "counsel-frank",
	})
	require.NoError(t, err)
	_, err = s.ActivateLegalHold(tenantCtx(), domain.ActivateLegalHoldParams{HoldID: hold.HoldID, ActivatedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)

	_, err = s.ApproveForDisposition(tenantCtx(), domain.ApproveForDispositionParams{RetentionStateID: st.RetentionStateID, ApprovedByPrincipalID: "approver-grace"})
	require.ErrorIs(t, err, domain.ErrRecordUnderLegalHold)

	// After release, approval succeeds — the hold, not a permanent
	// change, was the only thing blocking disposition.
	_, err = s.ReleaseLegalHold(tenantCtx(), domain.ReleaseLegalHoldParams{
		HoldID: hold.HoldID, ReleaseReason: "matter closed", ReleasedByPrincipalID: "counsel-frank", ReleaseApprovedByPrincipalID: "legal-ops-director-dana",
	})
	require.NoError(t, err)

	approved, err := s.ApproveForDisposition(tenantCtx(), domain.ApproveForDispositionParams{RetentionStateID: st.RetentionStateID, ApprovedByPrincipalID: "approver-grace"})
	require.NoError(t, err)
	require.Equal(t, domain.RetentionApprovedForDisposition, approved.State)
}

// ZS-SVC-S-001 §5.5: "release_approved_by: Separate release authority;
// self-release restrictions apply." The principal who issued (and here,
// also executes) the release cannot also be its own approver.
func TestDRC03_LegalHold_ReleaseRejectsSelfApproval(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "hold-self-release", string(domain.RecordClassLegalMatterRecord), "US-DE")

	hold, err := s.CreateLegalHold(tenantCtx(), domain.CreateLegalHoldParams{
		MatterRef: "Matter-2026-002", HoldReasonCode: "LITIGATION", RecordIDs: []string{rec.RecordID}, IssuedByPrincipalID: "counsel-frank",
	})
	require.NoError(t, err)
	_, err = s.ActivateLegalHold(tenantCtx(), domain.ActivateLegalHoldParams{HoldID: hold.HoldID, ActivatedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)

	_, err = s.ReleaseLegalHold(tenantCtx(), domain.ReleaseLegalHoldParams{
		HoldID: hold.HoldID, ReleaseReason: "self-release attempt", ReleasedByPrincipalID: "counsel-frank", ReleaseApprovedByPrincipalID: "counsel-frank",
	})
	require.ErrorIs(t, err, domain.ErrLegalHoldSelfRelease)

	// An independent approver still succeeds.
	released, err := s.ReleaseLegalHold(tenantCtx(), domain.ReleaseLegalHoldParams{
		HoldID: hold.HoldID, ReleaseReason: "matter closed", ReleasedByPrincipalID: "counsel-frank", ReleaseApprovedByPrincipalID: "legal-ops-director-dana",
	})
	require.NoError(t, err)
	require.Equal(t, domain.LegalHoldReleased, released.Status)
}

// Without any legal hold, the full dry-run pipeline reaches
// APPROVED_FOR_DISPOSITION — and nothing beyond it is reachable by any
// command in this wave.
func TestDRC03_FullDryRunPipeline_NoHold(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "full-pipeline", string(domain.RecordClassComplianceEvidence), "US-WA")
	rule := createActiveRetentionRule(t, s, string(domain.RecordClassComplianceEvidence), "US-WA", 7)

	trigger := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st, err := s.BindRetentionRule(tenantCtx(), domain.BindRetentionRuleParams{
		RecordID: rec.RecordID, RetentionRuleVersionID: rule.RetentionRuleVersionID, TriggerDate: &trigger, CreatedByPrincipalID: "operator-dave",
	})
	require.NoError(t, err)
	st, err = s.EvaluateDue(tenantCtx(), domain.EvaluateDueParams{RetentionStateID: st.RetentionStateID, AsOf: time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)})
	require.NoError(t, err)
	st, err = s.FlagForReview(tenantCtx(), domain.FlagForReviewParams{RetentionStateID: st.RetentionStateID, ReviewedByPrincipalID: "reviewer-eve", ReviewNotes: "no objections"})
	require.NoError(t, err)
	approved, err := s.ApproveForDisposition(tenantCtx(), domain.ApproveForDispositionParams{RetentionStateID: st.RetentionStateID, ApprovedByPrincipalID: "approver-grace"})
	require.NoError(t, err)
	require.Equal(t, domain.RetentionApprovedForDisposition, approved.State)

	// Structural proof this is the dry-run boundary: a raw UPDATE
	// attempting to move past APPROVED_FOR_DISPOSITION is rejected by
	// the database trigger — no command needs to exist for this to be
	// true.
	_, err = pool.Exec(context.Background(), `UPDATE record_retention_states SET state = 'DISPOSED' WHERE retention_state_id = $1`, st.RetentionStateID)
	require.Error(t, err, "moving past APPROVED_FOR_DISPOSITION should be rejected — this wave is dry-run only")
}

// A record cannot be bound to two retention rules.
func TestDRC03_BindRetentionRule_AlreadyBoundRefused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "double-bind", string(domain.RecordClassCorporateRecord), "US-DE")
	rule := createActiveRetentionRule(t, s, string(domain.RecordClassCorporateRecord), "US-DE", 365)

	_, err := s.BindRetentionRule(tenantCtx(), domain.BindRetentionRuleParams{
		RecordID: rec.RecordID, RetentionRuleVersionID: rule.RetentionRuleVersionID, CreatedByPrincipalID: "operator-dave",
	})
	require.NoError(t, err)

	_, err = s.BindRetentionRule(tenantCtx(), domain.BindRetentionRuleParams{
		RecordID: rec.RecordID, RetentionRuleVersionID: rule.RetentionRuleVersionID, CreatedByPrincipalID: "operator-dave",
	})
	require.ErrorIs(t, err, domain.ErrRecordAlreadyBound)
}

// A legal hold can cover multiple records; releasing it releases every
// target together.
func TestDRC03_LegalHold_MultipleTargetsReleasedTogether(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec1 := createTestRecord(t, s, "multi-target-1", string(domain.RecordClassLegalMatterRecord), "US-DE")
	rec2 := createTestRecord(t, s, "multi-target-2", string(domain.RecordClassLegalMatterRecord), "US-DE")

	hold, err := s.CreateLegalHold(tenantCtx(), domain.CreateLegalHoldParams{
		MatterRef: "Matter-multi", HoldReasonCode: "LITIGATION", RecordIDs: []string{rec1.RecordID, rec2.RecordID}, IssuedByPrincipalID: "counsel-frank",
	})
	require.NoError(t, err)
	_, err = s.ActivateLegalHold(tenantCtx(), domain.ActivateLegalHoldParams{HoldID: hold.HoldID, ActivatedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)

	targets, err := s.ListLegalHoldTargets(tenantCtx(), hold.HoldID)
	require.NoError(t, err)
	require.Len(t, targets, 2)
	for _, tgt := range targets {
		require.NotNil(t, tgt.AppliedAt)
		require.Nil(t, tgt.ReleasedAt)
	}

	_, err = s.ReleaseLegalHold(tenantCtx(), domain.ReleaseLegalHoldParams{
		HoldID: hold.HoldID, ReleaseReason: "resolved", ReleasedByPrincipalID: "counsel-frank", ReleaseApprovedByPrincipalID: "legal-ops-director-dana",
	})
	require.NoError(t, err)

	released, err := s.ListLegalHoldTargets(tenantCtx(), hold.HoldID)
	require.NoError(t, err)
	for _, tgt := range released {
		require.NotNil(t, tgt.ReleasedAt)
	}
}

// Adding a target to an already-ACTIVE hold applies it immediately —
// there is no asynchronous apply pipeline in this wave.
func TestDRC03_AddLegalHoldTarget_ToActiveHold_AppliesImmediately(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec1 := createTestRecord(t, s, "add-target-1", string(domain.RecordClassLegalMatterRecord), "US-DE")
	rec2 := createTestRecord(t, s, "add-target-2", string(domain.RecordClassLegalMatterRecord), "US-DE")

	hold, err := s.CreateLegalHold(tenantCtx(), domain.CreateLegalHoldParams{
		MatterRef: "Matter-add", HoldReasonCode: "LITIGATION", RecordIDs: []string{rec1.RecordID}, IssuedByPrincipalID: "counsel-frank",
	})
	require.NoError(t, err)
	_, err = s.ActivateLegalHold(tenantCtx(), domain.ActivateLegalHoldParams{HoldID: hold.HoldID, ActivatedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)

	target, err := s.AddLegalHoldTarget(tenantCtx(), domain.AddLegalHoldTargetParams{HoldID: hold.HoldID, RecordID: rec2.RecordID, CreatedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)
	require.NotNil(t, target.AppliedAt)
}

// Duplicate target on the same hold is refused.
func TestDRC03_AddLegalHoldTarget_DuplicateRefused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "dup-target", string(domain.RecordClassLegalMatterRecord), "US-DE")

	hold, err := s.CreateLegalHold(tenantCtx(), domain.CreateLegalHoldParams{
		MatterRef: "Matter-dup", HoldReasonCode: "LITIGATION", RecordIDs: []string{rec.RecordID}, IssuedByPrincipalID: "counsel-frank",
	})
	require.NoError(t, err)

	_, err = s.AddLegalHoldTarget(tenantCtx(), domain.AddLegalHoldTargetParams{HoldID: hold.HoldID, RecordID: rec.RecordID, CreatedByPrincipalID: "counsel-frank"})
	require.ErrorIs(t, err, domain.ErrLegalHoldTargetExists)
}

// Tenant isolation: cross-tenant reads are denied by RLS.
func TestDRC03_TenantIsolation(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rule := createActiveRetentionRule(t, s, string(domain.RecordClassAccountingWorkpaper), "US-CA", 30)

	otherTenantCtx := middleware.WithTenant(context.Background(), "99999999-9999-9999-9999-999999999999")
	_, err := s.GetRetentionRuleVersion(otherTenantCtx, rule.RetentionRuleVersionID)
	require.ErrorIs(t, err, domain.ErrRetentionRuleNotFound)
}
