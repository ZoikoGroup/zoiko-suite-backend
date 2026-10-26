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

// POST /v1/retention:evaluate and POST /v1/dispositions[/{id}:approve]
// (ZS-SVC-S-001 §9.2) against real Postgres.

const otherTenantID = "99999999-9999-9999-9999-999999999999"

func otherTenantCtx() context.Context { return middleware.WithTenant(tenantCtx(), otherTenantID) }

func activeRuleWith(t *testing.T, ctx context.Context, s *store.PgStore, class, jur string, days int, trigger domain.RetentionTriggerType, action domain.DispositionAction) *domain.RetentionRuleVersion {
	t.Helper()
	rr, err := s.CreateRetentionRuleVersion(ctx, domain.CreateRetentionRuleVersionParams{
		RecordClass: class, JurisdictionSelector: jur, LegalBasisRef: "statute-x", PurposeRef: "purpose-x",
		TriggerType: string(trigger), DurationDays: days, DispositionAction: string(action), CreatedByPrincipalID: "author-alice",
	})
	require.NoError(t, err)
	ap, err := s.ApproveRetentionRuleVersion(ctx, domain.ApproveRetentionRuleVersionParams{RetentionRuleVersionID: rr.RetentionRuleVersionID, ApprovedByPrincipalID: "approver-carol"})
	require.NoError(t, err)
	act, err := s.ActivateRetentionRuleVersion(ctx, ap.RetentionRuleVersionID)
	require.NoError(t, err)
	return act
}

// dueState walks a fresh record through bind -> DUE.
func dueState(t *testing.T, s *store.PgStore, label, class, jur string) (*domain.Record, *domain.RecordRetentionState) {
	t.Helper()
	rec := createTestRecord(t, s, label, class, jur)
	rule := activeRuleWith(t, tenantCtx(), s, class, jur, 1, domain.TriggerDeclaredAt, domain.DispositionDelete)
	trigger := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st, err := s.BindRetentionRule(tenantCtx(), domain.BindRetentionRuleParams{
		RecordID: rec.RecordID, RetentionRuleVersionID: rule.RetentionRuleVersionID, TriggerDate: &trigger, CreatedByPrincipalID: "operator-dave"})
	require.NoError(t, err)
	st, err = s.EvaluateDue(tenantCtx(), domain.EvaluateDueParams{RetentionStateID: st.RetentionStateID, AsOf: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)})
	require.NoError(t, err)
	require.Equal(t, domain.RetentionDue, st.State)
	return rec, st
}

func activateHold(t *testing.T, s *store.PgStore, recordID string) *domain.LegalHold {
	t.Helper()
	hold, err := s.CreateLegalHold(tenantCtx(), domain.CreateLegalHoldParams{
		MatterRef: "Matter-eval", HoldReasonCode: "LITIGATION", RecordIDs: []string{recordID}, IssuedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)
	_, err = s.ActivateLegalHold(tenantCtx(), domain.ActivateLegalHoldParams{HoldID: hold.HoldID, ActivatedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)
	return hold
}

// ── retention:evaluate ───────────────────────────────────────────────────────

func TestEvaluateRetention_NoApplicableRule_IsNotDefaulted(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec := createTestRecord(t, s, "eval-none", string(domain.RecordClassTaxReturnSupport), "ZZ-NONE")

	evs, err := s.EvaluateRetention(tenantCtx(), []string{rec.RecordID})
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, domain.ResolutionNoApplicableRule, evs[0].Resolution)
	require.Empty(t, evs[0].ApplicableRules)
	require.NotNil(t, evs[0].ApplicableRules, "an empty list, not null")
	require.Empty(t, evs[0].ConflictCode)
	require.Nil(t, evs[0].ScheduleState, "nothing is bound or invented")
	require.False(t, evs[0].LegalHold.Blocked)
	require.NotNil(t, evs[0].LegalHold.HoldIDs)
}

func TestEvaluateRetention_OneRule_Resolves(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec := createTestRecord(t, s, "eval-one", string(domain.RecordClassCorporateRecord), "US-DE")
	rule := activeRuleWith(t, tenantCtx(), s, string(domain.RecordClassCorporateRecord), "US-DE", 365, domain.TriggerContractEnd, domain.DispositionArchive)

	evs, err := s.EvaluateRetention(tenantCtx(), []string{rec.RecordID})
	require.NoError(t, err)
	require.Equal(t, domain.ResolutionResolved, evs[0].Resolution)
	require.Equal(t, string(domain.RecordClassCorporateRecord), evs[0].RecordClass)
	require.Equal(t, "US-DE", evs[0].JurisdictionScope)
	require.Len(t, evs[0].ApplicableRules, 1)
	got := evs[0].ApplicableRules[0]
	require.Equal(t, rule.RetentionRuleVersionID, got.RetentionRuleVersionID)
	require.Equal(t, domain.TriggerContractEnd, got.TriggerType)
	require.Equal(t, 365, got.DurationDays)
	require.Equal(t, domain.DispositionArchive, got.DispositionAction)
	require.Equal(t, "statute-x", got.LegalBasisRef)
}

func TestEvaluateRetention_IdenticalRules_ResolveWithEveryContributingRule(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec := createTestRecord(t, s, "eval-same", string(domain.RecordClassLegalContract), "US-NY")
	a := activeRuleWith(t, tenantCtx(), s, string(domain.RecordClassLegalContract), "US-NY", 730, domain.TriggerContractEnd, domain.DispositionReview)
	b := activeRuleWith(t, tenantCtx(), s, string(domain.RecordClassLegalContract), "US-NY", 730, domain.TriggerContractEnd, domain.DispositionReview)

	evs, err := s.EvaluateRetention(tenantCtx(), []string{rec.RecordID})
	require.NoError(t, err)
	require.Equal(t, domain.ResolutionResolved, evs[0].Resolution)
	require.Empty(t, evs[0].ConflictCode)
	ids := []string{evs[0].ApplicableRules[0].RetentionRuleVersionID, evs[0].ApplicableRules[1].RetentionRuleVersionID}
	require.ElementsMatch(t, []string{a.RetentionRuleVersionID, b.RetentionRuleVersionID}, ids)
}

func TestEvaluateRetention_DifferingRules_AreAConflict_AndNoWinnerIsChosen(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec := createTestRecord(t, s, "eval-conflict", string(domain.RecordClassEmploymentRecord), "GB")
	short := activeRuleWith(t, tenantCtx(), s, string(domain.RecordClassEmploymentRecord), "GB", 365, domain.TriggerEmploymentEnd, domain.DispositionDelete)
	long := activeRuleWith(t, tenantCtx(), s, string(domain.RecordClassEmploymentRecord), "GB", 2190, domain.TriggerEmploymentEnd, domain.DispositionDelete)

	evs, err := s.EvaluateRetention(tenantCtx(), []string{rec.RecordID})
	require.NoError(t, err)
	require.Equal(t, domain.ResolutionConflict, evs[0].Resolution)
	require.Equal(t, "RETENTION_CONFLICT", evs[0].ConflictCode)
	require.Len(t, evs[0].ApplicableRules, 2, "every contributing rule is reported, neither is chosen")
	require.ElementsMatch(t, []string{short.RetentionRuleVersionID, long.RetentionRuleVersionID},
		[]string{evs[0].ApplicableRules[0].RetentionRuleVersionID, evs[0].ApplicableRules[1].RetentionRuleVersionID})

	// A different DISPOSITION ACTION is a conflict too, even at equal duration.
	rec2 := createTestRecord(t, s, "eval-conflict-action", string(domain.RecordClassComplianceEvidence), "EU")
	activeRuleWith(t, tenantCtx(), s, string(domain.RecordClassComplianceEvidence), "EU", 100, domain.TriggerDeclaredAt, domain.DispositionDelete)
	activeRuleWith(t, tenantCtx(), s, string(domain.RecordClassComplianceEvidence), "EU", 100, domain.TriggerDeclaredAt, domain.DispositionAnonymize)
	evs, err = s.EvaluateRetention(tenantCtx(), []string{rec2.RecordID})
	require.NoError(t, err)
	require.Equal(t, domain.ResolutionConflict, evs[0].Resolution)
}

func TestEvaluateRetention_OnlyActiveRulesApply(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec := createTestRecord(t, s, "eval-draft", string(domain.RecordClassPayrollOutput), "US-CA")
	// A DRAFT and an APPROVED-but-not-activated rule exist; neither applies.
	_, err := s.CreateRetentionRuleVersion(tenantCtx(), domain.CreateRetentionRuleVersionParams{
		RecordClass: string(domain.RecordClassPayrollOutput), JurisdictionSelector: "US-CA", TriggerType: string(domain.TriggerDeclaredAt),
		DurationDays: 10, DispositionAction: string(domain.DispositionDelete), CreatedByPrincipalID: "author-alice"})
	require.NoError(t, err)
	drafted, err := s.CreateRetentionRuleVersion(tenantCtx(), domain.CreateRetentionRuleVersionParams{
		RecordClass: string(domain.RecordClassPayrollOutput), JurisdictionSelector: "US-CA", TriggerType: string(domain.TriggerDeclaredAt),
		DurationDays: 20, DispositionAction: string(domain.DispositionDelete), CreatedByPrincipalID: "author-alice"})
	require.NoError(t, err)
	_, err = s.ApproveRetentionRuleVersion(tenantCtx(), domain.ApproveRetentionRuleVersionParams{RetentionRuleVersionID: drafted.RetentionRuleVersionID, ApprovedByPrincipalID: "approver-carol"})
	require.NoError(t, err)

	evs, err := s.EvaluateRetention(tenantCtx(), []string{rec.RecordID})
	require.NoError(t, err)
	require.Equal(t, domain.ResolutionNoApplicableRule, evs[0].Resolution)
}

func TestEvaluateRetention_ReportsScheduleAndHold_AndMutatesNothing(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec, st := dueState(t, s, "eval-position", string(domain.RecordClassLegalMatterRecord), "US-TX")
	hold := activateHold(t, s, rec.RecordID)

	var outboxBefore int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&outboxBefore))

	evs, err := s.EvaluateRetention(tenantCtx(), []string{rec.RecordID})
	require.NoError(t, err)
	require.NotNil(t, evs[0].ScheduleState)
	require.Equal(t, st.RetentionStateID, evs[0].ScheduleState.RetentionStateID)
	require.Equal(t, domain.RetentionDue, evs[0].ScheduleState.State)
	require.True(t, evs[0].LegalHold.Blocked)
	require.Equal(t, []string{hold.HoldID}, evs[0].LegalHold.HoldIDs)

	var outboxAfter int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&outboxAfter))
	require.Equal(t, outboxBefore, outboxAfter, "evaluation publishes nothing")
	after, err := s.GetRecordRetentionState(tenantCtx(), st.RetentionStateID)
	require.NoError(t, err)
	require.Equal(t, domain.RetentionDue, after.State, "evaluation does not move the state")
}

func TestEvaluateRetention_IsTenantScoped(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec := createTestRecord(t, s, "eval-tenant", string(domain.RecordClassAccountingWorkpaper), "US-FL")
	// Another tenant's ACTIVE rule for the same class and selector must not apply.
	activeRuleWith(t, otherTenantCtx(), s, string(domain.RecordClassAccountingWorkpaper), "US-FL", 50, domain.TriggerDeclaredAt, domain.DispositionDelete)

	evs, err := s.EvaluateRetention(tenantCtx(), []string{rec.RecordID})
	require.NoError(t, err)
	require.Equal(t, domain.ResolutionNoApplicableRule, evs[0].Resolution, "another tenant's rule does not apply")

	// And another tenant cannot evaluate (or learn the entity of) this tenant's record.
	_, err = s.EvaluateRetention(otherTenantCtx(), []string{rec.RecordID})
	require.ErrorIs(t, err, domain.ErrRecordNotFound)
	_, err = s.RecordLegalEntities(otherTenantCtx(), []string{rec.RecordID})
	require.ErrorIs(t, err, domain.ErrRecordNotFound)

	ents, err := s.RecordLegalEntities(tenantCtx(), []string{rec.RecordID})
	require.NoError(t, err)
	require.Equal(t, rec.LegalEntityID, ents[rec.RecordID])
	_, err = s.RecordLegalEntities(tenantCtx(), []string{rec.RecordID, "00000000-0000-0000-0000-000000000000"})
	require.ErrorIs(t, err, domain.ErrRecordNotFound, "one unknown record fails the whole call")
}

// ── dispositions ─────────────────────────────────────────────────────────────

func TestDispositionCandidate_FromDue_MovesToReviewRequired_DeletesNothing(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec, st := dueState(t, s, "disp-ok", string(domain.RecordClassCorporateRecord), "US-DE")

	got, err := s.CreateDispositionCandidate(tenantCtx(), domain.CreateDispositionCandidateParams{
		RecordID: rec.RecordID, ExpectedState: domain.RetentionDue, ReviewNotes: "retention elapsed", CreatedByPrincipalID: "operator-dave"})
	require.NoError(t, err)
	require.Equal(t, st.RetentionStateID, got.RetentionStateID)
	require.Equal(t, domain.RetentionReviewRequired, got.State)
	require.Equal(t, "retention elapsed", got.ReviewNotes)
	require.NotNil(t, got.ReviewedByPrincipalID)
	require.Equal(t, "operator-dave", *got.ReviewedByPrincipalID)

	var docs int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM documents WHERE document_id::text = $1`, rec.DocumentID).Scan(&docs))
	require.Equal(t, 1, docs, "no content or metadata is deleted")
}

func TestDispositionCandidate_RefusalsMutateNothing(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())

	// Wrong expected state.
	rec, st := dueState(t, s, "disp-mismatch", string(domain.RecordClassCorporateRecord), "US-DE")
	_, err := s.CreateDispositionCandidate(tenantCtx(), domain.CreateDispositionCandidateParams{
		RecordID: rec.RecordID, ExpectedState: domain.RetentionActive, CreatedByPrincipalID: "operator-dave"})
	require.ErrorIs(t, err, domain.ErrRetentionStateMismatch)
	cur, _ := s.GetRecordRetentionState(tenantCtx(), st.RetentionStateID)
	require.Equal(t, domain.RetentionDue, cur.State)

	// Not DUE yet: ACTIVE with the correct expectation is still refused.
	rec2 := createTestRecord(t, s, "disp-active", string(domain.RecordClassCorporateRecord), "US-OR")
	rule := activeRuleWith(t, tenantCtx(), s, string(domain.RecordClassCorporateRecord), "US-OR", 3650, domain.TriggerDeclaredAt, domain.DispositionDelete)
	trigger := time.Now().UTC()
	act, err := s.BindRetentionRule(tenantCtx(), domain.BindRetentionRuleParams{RecordID: rec2.RecordID, RetentionRuleVersionID: rule.RetentionRuleVersionID, TriggerDate: &trigger, CreatedByPrincipalID: "operator-dave"})
	require.NoError(t, err)
	_, err = s.CreateDispositionCandidate(tenantCtx(), domain.CreateDispositionCandidateParams{
		RecordID: rec2.RecordID, ExpectedState: domain.RetentionActive, CreatedByPrincipalID: "operator-dave"})
	require.ErrorIs(t, err, domain.ErrRetentionStateNotDue)
	cur, _ = s.GetRecordRetentionState(tenantCtx(), act.RetentionStateID)
	require.Equal(t, domain.RetentionActive, cur.State)

	// No retention state at all.
	rec3 := createTestRecord(t, s, "disp-unbound", string(domain.RecordClassCorporateRecord), "US-WA")
	_, err = s.CreateDispositionCandidate(tenantCtx(), domain.CreateDispositionCandidateParams{
		RecordID: rec3.RecordID, ExpectedState: domain.RetentionDue, CreatedByPrincipalID: "operator-dave"})
	require.ErrorIs(t, err, domain.ErrRetentionStateNotFound)
}

func TestDispositionCandidate_LegalHoldBlocks_NamingTheHold(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec, st := dueState(t, s, "disp-hold", string(domain.RecordClassLegalMatterRecord), "US-DE")
	hold := activateHold(t, s, rec.RecordID)

	_, err := s.CreateDispositionCandidate(tenantCtx(), domain.CreateDispositionCandidateParams{
		RecordID: rec.RecordID, ExpectedState: domain.RetentionDue, CreatedByPrincipalID: "operator-dave"})
	require.ErrorIs(t, err, domain.ErrRecordUnderLegalHold)
	require.Contains(t, err.Error(), hold.HoldID, "the block names the hold (§11.1 DRC-008)")
	cur, _ := s.GetRecordRetentionState(tenantCtx(), st.RetentionStateID)
	require.Equal(t, domain.RetentionDue, cur.State, "nothing is mutated while a hold is active")
}

func TestDispositionCandidate_RepeatIsRefused_NotDuplicated(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec, _ := dueState(t, s, "disp-repeat", string(domain.RecordClassCorporateRecord), "US-DE")
	p := domain.CreateDispositionCandidateParams{RecordID: rec.RecordID, ExpectedState: domain.RetentionDue, CreatedByPrincipalID: "operator-dave"}
	_, err := s.CreateDispositionCandidate(tenantCtx(), p)
	require.NoError(t, err)
	_, err = s.CreateDispositionCandidate(tenantCtx(), p)
	require.ErrorIs(t, err, domain.ErrRetentionStateMismatch, "a retry sees REVIEW_REQUIRED, not DUE, and changes nothing")
}

func TestDispositionCandidate_IsTenantScoped(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec, _ := dueState(t, s, "disp-tenant", string(domain.RecordClassCorporateRecord), "US-DE")
	_, err := s.CreateDispositionCandidate(otherTenantCtx(), domain.CreateDispositionCandidateParams{
		RecordID: rec.RecordID, ExpectedState: domain.RetentionDue, CreatedByPrincipalID: "operator-dave"})
	require.ErrorIs(t, err, domain.ErrRetentionStateNotFound, "another tenant's record is indistinguishable from none")
}

func TestDispositionApprove_RecordsTheApprovalRef_AndHoldStillBlocks(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec, st := dueState(t, s, "disp-approve", string(domain.RecordClassLegalMatterRecord), "US-DE")
	_, err := s.CreateDispositionCandidate(tenantCtx(), domain.CreateDispositionCandidateParams{
		RecordID: rec.RecordID, ExpectedState: domain.RetentionDue, CreatedByPrincipalID: "operator-dave"})
	require.NoError(t, err)

	// A hold placed after the candidate still blocks approval, and names itself.
	hold := activateHold(t, s, rec.RecordID)
	_, err = s.ApproveForDisposition(tenantCtx(), domain.ApproveForDispositionParams{
		RetentionStateID: st.RetentionStateID, ApprovedByPrincipalID: "approver-grace", ApprovalRef: "wfc-decision-1"})
	require.ErrorIs(t, err, domain.ErrRecordUnderLegalHold)
	require.Contains(t, err.Error(), hold.HoldID)

	_, err = s.ReleaseLegalHold(tenantCtx(), domain.ReleaseLegalHoldParams{
		HoldID: hold.HoldID, ReleaseReason: "closed", ReleasedByPrincipalID: "counsel-frank", ReleaseApprovedByPrincipalID: "legal-ops-director-dana"})
	require.NoError(t, err)

	approved, err := s.ApproveForDisposition(tenantCtx(), domain.ApproveForDispositionParams{
		RetentionStateID: st.RetentionStateID, ApprovedByPrincipalID: "approver-grace", ApprovalRef: "wfc-decision-1"})
	require.NoError(t, err)
	require.Equal(t, domain.RetentionApprovedForDisposition, approved.State)
	require.NotNil(t, approved.DispositionApprovalRef)
	require.Equal(t, "wfc-decision-1", *approved.DispositionApprovalRef)

	// Approved is terminal for this wave: a second approval is refused.
	_, err = s.ApproveForDisposition(tenantCtx(), domain.ApproveForDispositionParams{
		RetentionStateID: st.RetentionStateID, ApprovedByPrincipalID: "approver-grace", ApprovalRef: "wfc-decision-2"})
	require.ErrorIs(t, err, domain.ErrRetentionStateNotReviewRequired)
}

func TestDispositionApprove_OldRouteStillWorksWithoutARef(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec, st := dueState(t, s, "disp-approve-old", string(domain.RecordClassCorporateRecord), "US-DE")
	_, err := s.FlagForReview(tenantCtx(), domain.FlagForReviewParams{RetentionStateID: st.RetentionStateID, ReviewedByPrincipalID: "reviewer-eve", ReviewNotes: "ok"})
	require.NoError(t, err)
	approved, err := s.ApproveForDisposition(tenantCtx(), domain.ApproveForDispositionParams{RetentionStateID: st.RetentionStateID, ApprovedByPrincipalID: "approver-grace"})
	require.NoError(t, err)
	require.Nil(t, approved.DispositionApprovalRef, "no reference was given, none is recorded")
	_ = rec
}

func TestDispositionApprove_IsTenantScoped(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec, st := dueState(t, s, "disp-approve-tenant", string(domain.RecordClassCorporateRecord), "US-DE")
	_, err := s.CreateDispositionCandidate(tenantCtx(), domain.CreateDispositionCandidateParams{
		RecordID: rec.RecordID, ExpectedState: domain.RetentionDue, CreatedByPrincipalID: "operator-dave"})
	require.NoError(t, err)
	_, err = s.ApproveForDisposition(otherTenantCtx(), domain.ApproveForDispositionParams{
		RetentionStateID: st.RetentionStateID, ApprovedByPrincipalID: "approver-grace", ApprovalRef: "x"})
	require.ErrorIs(t, err, domain.ErrRetentionStateNotFound)
}
