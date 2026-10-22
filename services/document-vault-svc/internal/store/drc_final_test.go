package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/store"
)

// §3.2 row_version / document_state, §5.2 effective_from/to, §9.3
// retention_conflict_detected — real PostgreSQL.

func TestRowVersion_StartsAtOne_AndIncrementsOnEveryChange(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	doc, v := newEventDoc(nil, "rv-basic")
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v, "c"))

	got, err := s.FindDocumentByID(tenantCtx(), doc.DocumentID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, got.RowVersion, "initial value")
	assert.Equal(t, "ACTIVE", got.DocumentState)

	v2 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("rv2"), StorageKey: "key-rv2", SizeBytes: 3, ContentType: "application/pdf", CreatedByPrincipalID: "p"}
	after, err := s.AddVersion(tenantCtx(), doc.DocumentID, v2, "c")
	require.NoError(t, err)
	assert.EqualValues(t, 2, after.RowVersion, "a successful mutation increments it")

	got, _ = s.FindDocumentByID(tenantCtx(), doc.DocumentID)
	assert.EqualValues(t, 2, got.RowVersion)
}

func TestRowVersion_CannotBeSetByAClient_AndConcurrentWritesDoNotLoseIncrements(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := newEventDoc(nil, "rv-conc")
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v, "c"))
	ctx := context.Background()

	// An UPDATE that tries to write its own token gets the next value instead.
	_, err := pool.Exec(ctx, `UPDATE documents SET row_version = 500 WHERE document_id::text = $1`, doc.DocumentID)
	require.NoError(t, err)
	var rv int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT row_version FROM documents WHERE document_id::text = $1`, doc.DocumentID).Scan(&rv))
	assert.EqualValues(t, 2, rv, "the token is owned by the database, not by the writer")

	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := pool.Exec(ctx, `UPDATE documents SET updated_at = now() WHERE document_id::text = $1`, doc.DocumentID)
			assert.NoError(t, e)
		}()
	}
	wg.Wait()
	require.NoError(t, pool.QueryRow(ctx, `SELECT row_version FROM documents WHERE document_id::text = $1`, doc.DocumentID).Scan(&rv))
	assert.EqualValues(t, 2+writers, rv, "concurrent updates each increment exactly once")
}

func TestRowVersion_IsTenantScoped(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	doc, v := newEventDoc(nil, "rv-tenant")
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v, "c"))
	_, err := s.FindDocumentByID(otherTenantCtx(), doc.DocumentID)
	require.ErrorIs(t, err, domain.ErrDocumentNotFound, "another tenant cannot read the document or its token")
}

func TestDocumentStateFor_MapsOnlyIdenticalStates(t *testing.T) {
	assert.Equal(t, "ACTIVE", domain.DocumentStateFor(domain.StatusActive))
	assert.Equal(t, "SUPERSEDED", domain.DocumentStateFor(domain.StatusSuperseded))
	for _, st := range []domain.DocumentStatus{domain.StatusRetained, domain.StatusPurgePending, domain.StatusArchived} {
		assert.Empty(t, domain.DocumentStateFor(st), "%s has no §3.2 equivalent and is not guessed", st)
	}
}

// ── effective-dated rules ────────────────────────────────────────────────────

func windowRule(t *testing.T, s *store.PgStore, class, jur string, days int, from, to *time.Time) *domain.RetentionRuleVersion {
	t.Helper()
	rr, err := s.CreateRetentionRuleVersion(tenantCtx(), domain.CreateRetentionRuleVersionParams{
		RecordClass: class, JurisdictionSelector: jur, LegalBasisRef: "statute-x", PurposeRef: "purpose-x",
		TriggerType: string(domain.TriggerDeclaredAt), DurationDays: days, DispositionAction: string(domain.DispositionDelete),
		CreatedByPrincipalID: "author-alice", EffectiveFrom: from, EffectiveTo: to})
	require.NoError(t, err)
	ap, err := s.ApproveRetentionRuleVersion(tenantCtx(), domain.ApproveRetentionRuleVersionParams{RetentionRuleVersionID: rr.RetentionRuleVersionID, ApprovedByPrincipalID: "approver-carol"})
	require.NoError(t, err)
	act, err := s.ActivateRetentionRuleVersion(tenantCtx(), ap.RetentionRuleVersionID)
	require.NoError(t, err)
	return act
}

func TestEvaluate_EffectiveWindow_DecidesApplicability(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	rec := createTestRecord(t, s, "win-rec", string(domain.RecordClassCorporateRecord), "US-WIN")
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := windowRule(t, s, string(domain.RecordClassCorporateRecord), "US-WIN", 100, &t0, &t1) // [2024, 2025)
	cur := windowRule(t, s, string(domain.RecordClassCorporateRecord), "US-WIN", 200, &t1, &t2) // [2025, 2026)
	open := windowRule(t, s, string(domain.RecordClassCorporateRecord), "US-WIN", 300, nil, nil)
	_ = open

	at := func(ts time.Time) []string {
		evs, err := s.EvaluateRetentionAt(tenantCtx(), domain.EvaluateRetentionParams{RecordIDs: []string{rec.RecordID}, AsOf: ts, ActorPrincipalID: "p"})
		require.NoError(t, err)
		require.True(t, evs[0].AsOf.Equal(ts))
		var ids []string
		for _, r := range evs[0].ApplicableRules {
			ids = append(ids, r.RetentionRuleVersionID)
		}
		return ids
	}
	assert.ElementsMatch(t, []string{old.RetentionRuleVersionID, open.RetentionRuleVersionID}, at(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)))
	assert.ElementsMatch(t, []string{cur.RetentionRuleVersionID, open.RetentionRuleVersionID}, at(t1), "effective_from is inclusive, effective_to exclusive")
	assert.ElementsMatch(t, []string{open.RetentionRuleVersionID}, at(t2), "a rule no longer in its window drops out")
	assert.ElementsMatch(t, []string{open.RetentionRuleVersionID}, at(time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)), "a rule not yet effective is not applied")
}

func TestEffectiveWindow_Validation_AndImmutability(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	a := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := s.CreateRetentionRuleVersion(tenantCtx(), domain.CreateRetentionRuleVersionParams{
		RecordClass: string(domain.RecordClassCorporateRecord), JurisdictionSelector: "US-V", TriggerType: string(domain.TriggerDeclaredAt),
		DurationDays: 1, DispositionAction: string(domain.DispositionDelete), CreatedByPrincipalID: "x", EffectiveFrom: &a, EffectiveTo: &a})
	require.ErrorIs(t, err, domain.ErrInvalidEffectiveWindow, "an empty or inverted window is refused")

	b := a.Add(24 * time.Hour)
	rule := windowRule(t, s, string(domain.RecordClassCorporateRecord), "US-V", 5, &a, &b)
	_, err = pool.Exec(context.Background(), `UPDATE retention_rule_versions SET effective_to = $2 WHERE retention_rule_version_id::text = $1`,
		rule.RetentionRuleVersionID, b.Add(time.Hour))
	require.Error(t, err, "an ACTIVE rule's window cannot be edited")
	got, err := s.GetRetentionRuleVersion(tenantCtx(), rule.RetentionRuleVersionID)
	require.NoError(t, err)
	require.NotNil(t, got.EffectiveFrom)
	assert.True(t, got.EffectiveFrom.Equal(a))
}

// ── retention_conflict_detected ──────────────────────────────────────────────

func TestEvaluate_Conflict_PublishesTheEventOncePerRuleSet(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "cf-rec", string(domain.RecordClassPayrollOutput), "US-CF")
	r1 := windowRule(t, s, string(domain.RecordClassPayrollOutput), "US-CF", 100, nil, nil)
	r2 := windowRule(t, s, string(domain.RecordClassPayrollOutput), "US-CF", 200, nil, nil)
	clearOutbox(t, pool)

	// The read-only form publishes nothing.
	_, err := s.EvaluateRetention(tenantCtx(), []string{rec.RecordID})
	require.NoError(t, err)
	assert.Empty(t, eventsOfType(t, pool, "retention_conflict_detected"))

	eval := func() domain.RetentionEvaluation {
		evs, err := s.EvaluateRetentionAt(tenantCtx(), domain.EvaluateRetentionParams{RecordIDs: []string{rec.RecordID}, ActorPrincipalID: "p"})
		require.NoError(t, err)
		return evs[0]
	}
	ev := eval()
	require.Equal(t, domain.ResolutionConflict, ev.Resolution)

	got := eventsOfType(t, pool, "retention_conflict_detected")
	require.Len(t, got, 1)
	assert.Equal(t, rec.RecordID, got[0].Payload["record_id"])
	assert.Equal(t, "RETENTION_CONFLICT", got[0].Payload["conflict_code"])
	assert.ElementsMatch(t, []any{r1.RetentionRuleVersionID, r2.RetentionRuleVersionID}, got[0].Payload["contributing_rule_ids"])

	eval()
	assert.Len(t, eventsOfType(t, pool, "retention_conflict_detected"), 1, "the same conflict is not published twice")

	r3 := windowRule(t, s, string(domain.RecordClassPayrollOutput), "US-CF", 300, nil, nil)
	eval()
	got = eventsOfType(t, pool, "retention_conflict_detected")
	require.Len(t, got, 2, "a changed rule set is a new detection")
	assert.Contains(t, got[1].Payload["contributing_rule_ids"], r3.RetentionRuleVersionID)
}

func TestEvaluate_NoConflict_PublishesNoConflictEvent(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "cf-none", string(domain.RecordClassPayrollOutput), "US-OK")
	windowRule(t, s, string(domain.RecordClassPayrollOutput), "US-OK", 100, nil, nil)
	clearOutbox(t, pool)
	_, err := s.EvaluateRetentionAt(tenantCtx(), domain.EvaluateRetentionParams{RecordIDs: []string{rec.RecordID}, ActorPrincipalID: "p"})
	require.NoError(t, err)
	assert.Empty(t, eventsOfType(t, pool, "retention_conflict_detected"))
}

// ── cross-tenant declaration (§11.2: fails before any object read) ───────────

func TestDeclareRecord_CrossTenantVersion_IsRefusedAndWritesNothing(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := newEventDoc(nil, "xt-declare")
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v, "c"))
	clearOutbox(t, pool)

	_, _, err := s.DeclareRecordV2(otherTenantCtx(), domain.DeclareRecordV2Params{
		DocumentID: doc.DocumentID, DocumentVersionID: v.DocumentVersionID, LegalEntityID: doc.LegalEntityID,
		RecordClass: string(domain.RecordClassLegalContract), JurisdictionScope: "US-DE", DeclaredByPrincipalID: "p",
		DeclarationReason: "x", BusinessContext: "y", CorrelationID: "c"})
	require.ErrorIs(t, err, domain.ErrDocumentVersionNotFoundForRecord, "another tenant's version is indistinguishable from none")

	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM records WHERE document_id::text = $1`, doc.DocumentID).Scan(&n))
	assert.Zero(t, n)
	assert.Empty(t, eventsOfType(t, pool, "record.declared"))

	// The owner still can.
	_, _, err = s.DeclareRecordV2(tenantCtx(), domain.DeclareRecordV2Params{
		DocumentID: doc.DocumentID, DocumentVersionID: v.DocumentVersionID, LegalEntityID: doc.LegalEntityID,
		RecordClass: string(domain.RecordClassLegalContract), JurisdictionScope: "US-DE", DeclaredByPrincipalID: "p",
		DeclarationReason: "x", BusinessContext: "y", CorrelationID: "c2"})
	require.NoError(t, err)
}

func TestCurrentVersion_TracksTheNewestVersion(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	doc, v1 := newEventDoc(nil, "cv-track")
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v1, "c"))
	v2 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("cv2"), StorageKey: "key-cv2", SizeBytes: 3, ContentType: "application/pdf", CreatedByPrincipalID: "p"}
	after, err := s.AddVersion(tenantCtx(), doc.DocumentID, v2, "c")
	require.NoError(t, err)
	got, err := s.FindVersion(tenantCtx(), doc.DocumentID, after.CurrentVersion)
	require.NoError(t, err)
	assert.Equal(t, v2.DocumentVersionID, got.DocumentVersionID, "the pointer follows the document's current version")
	_, err = s.FindVersion(otherTenantCtx(), doc.DocumentID, after.CurrentVersion)
	require.Error(t, err, "and is tenant-scoped")
}
