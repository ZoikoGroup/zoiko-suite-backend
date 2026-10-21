package store

// Real-PostgreSQL tests for the gaps found by the 64.7% compliance audit.
// Same harness as the other integration tests: a fresh database per test, all
// migrations applied (including 000006), connected as a NOSUPERUSER
// NOBYPASSRLS role. They skip without TEST_DATABASE_URL and FAIL (not skip)
// when REQUIRE_DB_TESTS=1.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/privacy-consent-svc/internal/domain"
	"zoiko.io/privacy-consent-svc/internal/testdb"
)

func creq(subject, purpose, noticeVersion, action string) domain.RecordConsentRequest {
	return domain.RecordConsentRequest{SubjectRef: subject, PurposeID: purpose, NoticeVersionID: noticeVersion, Action: action, CaptureChannel: "WEB"}
}

func record(t *testing.T, s *PgStore, tenant string, req domain.RecordConsentRequest) *domain.ConsentReceipt {
	t.Helper()
	r, err := s.RecordConsent(tctx(tenant), tenant, req, "actor-1", "corr-"+uuid.NewString())
	if err != nil {
		t.Fatalf("RecordConsent: %v", err)
	}
	return r
}

func statusFor(t *testing.T, s *PgStore, tenant, subject, purpose string) domain.ConsentStatus {
	t.Helper()
	res, err := s.ResolveConsentStatus(tctx(tenant), subject, purpose)
	if err != nil {
		t.Fatalf("ResolveConsentStatus: %v", err)
	}
	return res.Status
}

func publishedNotice(t *testing.T, s *PgStore, tenant string) string {
	t.Helper()
	return noticeIn(t, s, tenant, domain.NoticeStatusPublished).NoticeVersionID
}

// consentEvents returns the payload of every privacy.consent.changed event for
// a subject, oldest first.
func consentEvents(t *testing.T, db *testdb.DB, subject string) []map[string]any {
	t.Helper()
	rows, err := db.Admin.Query(context.Background(),
		`SELECT payload->'payload' FROM event_outbox WHERE event_type='privacy.consent.changed' AND entity_id=$1 ORDER BY created_at, outbox_event_id`, subject)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func countWhere(t *testing.T, db *testdb.DB, table, where string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Admin.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ── GAP 1 ────────────────────────────────────────────────────────────────────

func TestGap1_ReplayOfActiveConsent_ReturnsOriginal_NoNewEvidenceNoEvent(t *testing.T) {
	s, db := newStore(t)
	nv := publishedNotice(t, s, tenantA)
	a := record(t, s, tenantA, creq("s1", "p1", nv, "GRANTED"))
	b := record(t, s, tenantA, creq("s1", "p1", nv, "GRANTED"))
	if a.ConsentReceiptID != b.ConsentReceiptID {
		t.Fatalf("PRV-N04: replay of an active consent must return the original receipt")
	}
	if n := countWhere(t, db, "consent_receipts", "subject_ref='s1'"); n != 1 {
		t.Fatalf("replay created duplicate evidence: %d receipts", n)
	}
	if n := len(consentEvents(t, db, "s1")); n != 1 {
		t.Fatalf("replay emitted an event: %d events", n)
	}
}

func TestGap1_NewConsentAfterInvalidation_BecomesGranted_OldEvidencePreserved(t *testing.T) {
	s, db := newStore(t)
	nv := publishedNotice(t, s, tenantA)
	first := record(t, s, tenantA, creq("s2", "p1", nv, "GRANTED"))
	if _, err := s.InvalidateConsent(tctx(tenantA), first.ConsentReceiptID, "channel compromised", "SECOPS", "ops", "corr-i"); err != nil {
		t.Fatal(err)
	}
	if got := statusFor(t, s, tenantA, "s2", "p1"); got != domain.ConsentStatusInvalidated {
		t.Fatalf("precondition: %s", got)
	}

	second := record(t, s, tenantA, creq("s2", "p1", nv, "GRANTED"))
	if second.ConsentReceiptID == first.ConsentReceiptID {
		t.Fatalf("the INVALIDATED receipt was returned as if it were the live consent")
	}
	if got := statusFor(t, s, tenantA, "s2", "p1"); got != domain.ConsentStatusGranted {
		t.Fatalf("the new receipt must become GRANTED, got %s", got)
	}
	// Old evidence: still there, byte-for-byte, and still invalidated.
	if n := countWhere(t, db, "consent_receipts", "subject_ref='s2'"); n != 2 {
		t.Fatalf("want 2 receipts retained, have %d", n)
	}
	if n := countWhere(t, db, "consent_invalidations", "consent_receipt_id=$1", first.ConsentReceiptID); n != 1 {
		t.Fatalf("the original invalidation record was lost")
	}
	var h string
	_ = db.Admin.QueryRow(context.Background(), `SELECT receipt_integrity_hash FROM consent_receipts WHERE consent_receipt_id=$1`, first.ConsentReceiptID).Scan(&h)
	if h != first.ReceiptIntegrityHash {
		t.Fatalf("the original receipt changed")
	}
	// A further replay now returns the NEW receipt, not the old one.
	if again := record(t, s, tenantA, creq("s2", "p1", nv, "GRANTED")); again.ConsentReceiptID != second.ConsentReceiptID {
		t.Fatalf("replay after re-grant must return the new live receipt")
	}
}

func TestGap1_NewConsentAfterExpiry_BecomesGranted_OldEvidencePreserved(t *testing.T) {
	s, db := newStore(t)
	nv := publishedNotice(t, s, tenantA)
	past := time.Now().Add(-time.Hour).UTC()
	old := creq("s3", "p1", nv, "GRANTED")
	old.ExpiresAt = &past
	first := record(t, s, tenantA, old)
	if got := statusFor(t, s, tenantA, "s3", "p1"); got != domain.ConsentStatusExpired {
		t.Fatalf("precondition: %s", got)
	}

	renewed := creq("s3", "p1", nv, "GRANTED")
	future := time.Now().Add(24 * time.Hour).UTC()
	renewed.ExpiresAt = &future
	second := record(t, s, tenantA, renewed)
	if second.ConsentReceiptID == first.ConsentReceiptID {
		t.Fatalf("the EXPIRED receipt was returned as if it were the live consent")
	}
	if got := statusFor(t, s, tenantA, "s3", "p1"); got != domain.ConsentStatusGranted {
		t.Fatalf("want GRANTED, got %s", got)
	}
	if n := countWhere(t, db, "consent_receipts", "subject_ref='s3'"); n != 2 {
		t.Fatalf("want both receipts retained, have %d", n)
	}
}

func TestGap1_AfterWithdrawalAndAfterALaterDeny_ANewReceiptIsWritten(t *testing.T) {
	s, _ := newStore(t)
	nv := publishedNotice(t, s, tenantA)
	g1 := record(t, s, tenantA, creq("s4", "p1", nv, "GRANTED"))
	if _, err := s.WithdrawConsent(tctx(tenantA), g1.ConsentReceiptID, "ACCOUNT", "u", "c"); err != nil {
		t.Fatal(err)
	}
	g2 := record(t, s, tenantA, creq("s4", "p1", nv, "GRANTED"))
	if g2.ConsentReceiptID == g1.ConsentReceiptID {
		t.Fatalf("a grant after a withdrawal reused the withdrawn receipt")
	}
	record(t, s, tenantA, creq("s4", "p1", nv, "DENIED"))
	g3 := record(t, s, tenantA, creq("s4", "p1", nv, "GRANTED"))
	if g3.ConsentReceiptID == g2.ConsentReceiptID {
		t.Fatalf("a stale earlier GRANT answered a request made after a later DENY")
	}
	if got := statusFor(t, s, tenantA, "s4", "p1"); got != domain.ConsentStatusGranted {
		t.Fatalf("want GRANTED, got %s", got)
	}
}

// ── GAP 5: the stored digest covers what is stored ───────────────────────────

func storedReceipt(t *testing.T, db *testdb.DB, id string) *domain.ConsentReceipt {
	t.Helper()
	ctx := context.Background()
	r, err := scanConsentReceipt(db.Admin.QueryRow(ctx, `SELECT `+consentReceiptColumns+` FROM consent_receipts WHERE consent_receipt_id=$1`, id))
	if err != nil {
		t.Fatalf("read stored receipt: %v", err)
	}
	tx, err := db.Admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if r.ConsentRequest, err = loadConsentRequest(ctx, tx, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func richRequest(subject, purpose, nv string) domain.RecordConsentRequest {
	imported := time.Now().Add(-3 * 365 * 24 * time.Hour).UTC()
	expires := time.Now().Add(365 * 24 * time.Hour).UTC()
	return domain.RecordConsentRequest{
		SubjectRef: subject, PurposeID: purpose, NoticeVersionID: nv, Action: "GRANTED", CaptureChannel: "BRANCH",
		IsProxy: true, RepresentativeSubjectRef: "guardian-1", RepresentativeAuthorityRef: "poa-77", RepresentativeEvidence: "notarised",
		AffirmativeActionType: "SIGNED_FORM", AffirmativeEvidence: "form-123", AgeBand: "MINOR",
		IsOfflineImport: true, ImportSource: "legacy-crm", ImportCaptureMethod: "paper-form", ImportEvidenceRef: "scan-9", ImportedAt: &imported,
		ExpiresAt: &expires,
		ConsentRequest: &domain.ConsentRequest{
			PurposeID: purpose, Scope: "email", DataContext: "contact", RecipientContext: "processor", TechnologyContext: "cookie",
			Vendor: "acme", StorageLifetime: "12m", JurisdictionRuleRef: "rule-1", RequiredStatus: "OPTIONAL", PolicyPackage: "pkg-1",
		},
	}
}

func TestGap5_StoredIntegrityHash_RecomputesFromTheStoredRow(t *testing.T) {
	s, db := newStore(t)
	nv := publishedNotice(t, s, tenantA)
	req := richRequest("s5", "p1", nv)
	req.ConsentRequest.NoticeVersionID = nv
	rec := record(t, s, tenantA, req)

	if domain.IntegrityScheme(rec.ReceiptIntegrityHash) != "v2" {
		t.Fatalf("new receipts must carry the v2 digest, got %q", rec.ReceiptIntegrityHash)
	}
	stored := storedReceipt(t, db, rec.ConsentReceiptID)
	if got := stored.ComputeIntegrityHash(); got != stored.ReceiptIntegrityHash {
		t.Fatalf("the digest does not match the row as stored: recomputed %s, stored %s", got, stored.ReceiptIntegrityHash)
	}
	// A plain receipt (no optional evidence at all) also round-trips.
	plain := record(t, s, tenantA, creq("s5b", "p1", nv, "DENIED"))
	sp := storedReceipt(t, db, plain.ConsentReceiptID)
	if sp.ComputeIntegrityHash() != sp.ReceiptIntegrityHash {
		t.Fatalf("a minimal receipt does not round-trip")
	}
}

// The point of widening the digest: tampering with ANY of the evidence the
// audit named is now detectable by recomputation.
func TestGap5_TamperingWithEvidenceFieldsIsDetected(t *testing.T) {
	s, db := newStore(t)
	nv := publishedNotice(t, s, tenantA)
	req := richRequest("s5t", "p1", nv)
	rec := record(t, s, tenantA, req)
	ctx := context.Background()

	if _, err := db.Admin.Exec(ctx, `ALTER TABLE consent_receipts DISABLE TRIGGER consent_receipts_append_only`); err != nil {
		t.Skipf("cannot disable the append-only trigger to simulate tampering: %v", err)
	}
	if _, err := db.Admin.Exec(ctx, `ALTER TABLE consent_requests DISABLE TRIGGER consent_requests_append_only`); err != nil {
		t.Fatal(err)
	}
	tampers := map[string]string{
		"proxy representative":      `UPDATE consent_receipts SET representative_subject_ref='someone-else' WHERE consent_receipt_id=$1`,
		"proxy authority":           `UPDATE consent_receipts SET representative_authority_ref='forged' WHERE consent_receipt_id=$1`,
		"affirmative evidence":      `UPDATE consent_receipts SET affirmative_evidence='forged' WHERE consent_receipt_id=$1`,
		"affirmative action type":   `UPDATE consent_receipts SET affirmative_action_type='PRESELECTED' WHERE consent_receipt_id=$1`,
		"import source":             `UPDATE consent_receipts SET import_source='forged' WHERE consent_receipt_id=$1`,
		"import capture method":     `UPDATE consent_receipts SET import_capture_method='forged' WHERE consent_receipt_id=$1`,
		"import evidence ref":       `UPDATE consent_receipts SET import_evidence_ref='forged' WHERE consent_receipt_id=$1`,
		"imported_at":               `UPDATE consent_receipts SET imported_at = imported_at + interval '1 day' WHERE consent_receipt_id=$1`,
		"expires_at":                `UPDATE consent_receipts SET expires_at = expires_at + interval '1 day' WHERE consent_receipt_id=$1`,
		"age band":                  `UPDATE consent_receipts SET age_band='ADULT' WHERE consent_receipt_id=$1`,
		"action":                    `UPDATE consent_receipts SET action='DENIED' WHERE consent_receipt_id=$1`,
		"consent request scope":     `UPDATE consent_requests SET scope='forged' WHERE consent_receipt_id=$1`,
		"consent request vendor":    `UPDATE consent_requests SET vendor='forged' WHERE consent_receipt_id=$1`,
		"consent request policy":    `UPDATE consent_requests SET policy_package='forged' WHERE consent_receipt_id=$1`,
	}
	for name, stmt := range tampers {
		tx, err := db.Admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, stmt, rec.ConsentReceiptID); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: %v", name, err)
		}
		r, err := scanConsentReceipt(tx.QueryRow(ctx, `SELECT `+consentReceiptColumns+` FROM consent_receipts WHERE consent_receipt_id=$1`, rec.ConsentReceiptID))
		if err != nil {
			t.Fatal(err)
		}
		if r.ConsentRequest, err = loadConsentRequest(ctx, tx, r); err != nil {
			t.Fatal(err)
		}
		if r.ComputeIntegrityHash() == r.ReceiptIntegrityHash {
			t.Errorf("tampering with %s is NOT detected by the integrity hash", name)
		}
		_ = tx.Rollback(ctx) // leave the evidence intact for the next case
	}
}

// ── GAP 4: complete offline import is stored, and its event carries the original time ──

func TestGap4_CompleteOfflineImport_StoredWithProvenance_AndEventUsesTheOriginalTime(t *testing.T) {
	s, db := newStore(t)
	nv := publishedNotice(t, s, tenantA)
	req := richRequest("s4i", "p1", nv)
	rec := record(t, s, tenantA, req)

	st := storedReceipt(t, db, rec.ConsentReceiptID)
	if !st.IsOfflineImport || derefS(st.ImportSource) != "legacy-crm" || derefS(st.ImportCaptureMethod) != "paper-form" ||
		derefS(st.ImportEvidenceRef) != "scan-9" || st.ImportedAt == nil {
		t.Fatalf("provenance not stored: %+v", st)
	}
	// "Never fabricate current-platform consent timestamps": created_at is when we
	// recorded it; the event's effective time is when the consent actually happened.
	ev := consentEvents(t, db, "s4i")
	if len(ev) != 1 {
		t.Fatalf("want 1 event, got %d", len(ev))
	}
	eff, err := time.Parse(time.RFC3339Nano, ev[0]["effective_time"].(string))
	if err != nil || !eff.Equal(*st.ImportedAt) {
		t.Fatalf("effective_time = %v, want the original consent time %v", eff, st.ImportedAt)
	}
	if !st.CreatedAt.After(*st.ImportedAt) {
		t.Fatalf("created_at must be the recording time, after the import time")
	}
}

func derefS(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ── GAP 8: privacy.consent.changed (§19) ─────────────────────────────────────

func TestGap8_ConsentChangedPayload_CarriesTheSection19Fields_ForGrantDenyWithdrawInvalidate(t *testing.T) {
	s, db := newStore(t)
	nv := publishedNotice(t, s, tenantA)
	ctx := tctx(tenantA)

	grant := creq("s8", "p-8", nv, "GRANTED")
	grant.ConsentRequest = &domain.ConsentRequest{PurposeID: "p-8", Scope: "email"}
	g1 := record(t, s, tenantA, grant)
	record(t, s, tenantA, grant) // replay: no event
	w, err := s.WithdrawConsent(ctx, g1.ConsentReceiptID, "ACCOUNT", "u-w", "corr-w")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WithdrawConsent(ctx, g1.ConsentReceiptID, "ACCOUNT", "u-w", "corr-w"); err != nil { // replay: no event
		t.Fatal(err)
	}
	g2 := record(t, s, tenantA, creq("s8", "p-8", nv, "GRANTED"))
	d := record(t, s, tenantA, creq("s8", "p-8", nv, "DENIED"))
	inv, err := s.InvalidateConsent(ctx, d.ConsentReceiptID, "fraud", "SECOPS", "u-i", "corr-i")
	if err != nil {
		t.Fatal(err)
	}

	ev := consentEvents(t, db, "s8")
	type step struct {
		change, old, new_, ref string
	}
	want := []step{
		{"GRANTED", "NOT_REQUESTED", "GRANTED", g1.ConsentReceiptID},
		{"WITHDRAWN", "GRANTED", "WITHDRAWN", g1.ConsentReceiptID},
		{"GRANTED", "WITHDRAWN", "GRANTED", g2.ConsentReceiptID},
		{"DENIED", "GRANTED", "DENIED", d.ConsentReceiptID},
		{"INVALIDATED", "DENIED", "INVALIDATED", d.ConsentReceiptID},
	}
	if len(ev) != len(want) {
		t.Fatalf("want exactly %d events (replays emit none), got %d: %v", len(want), len(ev), ev)
	}
	for i, w := range want {
		e := ev[i]
		if e["change_type"] != w.change || e["old_state"] != w.old || e["new_state"] != w.new_ || e["receipt_id"] != w.ref {
			t.Errorf("event %d = change %v old %v new %v receipt %v, want %+v", i, e["change_type"], e["old_state"], e["new_state"], e["receipt_id"], w)
		}
		for _, f := range []string{"subject_ref", "purpose_id", "effective_time"} {
			if e[f] == nil || e[f] == "" {
				t.Errorf("event %d lacks %s: %v", i, f, e)
			}
		}
		if e["subject_ref"] != "s8" || e["purpose_id"] != "p-8" {
			t.Errorf("event %d subject/purpose wrong: %v", i, e)
		}
	}
	if ev[0]["scope"] != "email" || ev[1]["scope"] != "email" {
		t.Errorf("purpose/scope: the grant and its withdrawal must carry the receipt's scope: %v / %v", ev[0]["scope"], ev[1]["scope"])
	}
	if ev[1]["withdrawal_id"] != w.WithdrawalReceiptID || ev[1]["propagation_correlation"] != w.PropagationCorrelationID || ev[1]["propagation_correlation"] == "" {
		t.Errorf("withdrawal reference/propagation missing: %v", ev[1])
	}
	if ev[4]["invalidation_id"] != inv.ConsentInvalidationID {
		t.Errorf("invalidation reference missing: %v", ev[4])
	}
	// Effective time of a withdrawal is its own recording time.
	if got, _ := time.Parse(time.RFC3339Nano, ev[1]["effective_time"].(string)); !got.Equal(w.CreatedAt) {
		t.Errorf("withdrawal effective_time %v != %v", got, w.CreatedAt)
	}
	// Still exactly one outbox row per event (no duplicates) and all enqueued in-transaction.
	if n := countWhere(t, db, "event_outbox", "event_type='privacy.consent.changed' AND entity_id='s8'"); n != len(want) {
		t.Errorf("outbox rows = %d, want %d", n, len(want))
	}
}

func TestGap8_WithdrawingAnOlderReceipt_CanLeaveTheStateUnchanged_AndSaysSo(t *testing.T) {
	s, db := newStore(t)
	nv := publishedNotice(t, s, tenantA)
	g1 := record(t, s, tenantA, creq("s8b", "p1", nv, "GRANTED"))
	record(t, s, tenantA, creq("s8b", "p1", nv, "DENIED")) // the DENY is now the live outcome
	if _, err := s.WithdrawConsent(tctx(tenantA), g1.ConsentReceiptID, "ACCOUNT", "u", "c"); err != nil {
		t.Fatal(err)
	}
	ev := consentEvents(t, db, "s8b")
	last := ev[len(ev)-1]
	if last["change_type"] != "WITHDRAWN" || last["old_state"] != "DENIED" || last["new_state"] != "DENIED" {
		t.Fatalf("old/new state describe the DERIVED status of the pair, which a withdrawn-but-superseded grant does not change: %v", last)
	}
}

// ── GAP 2: strict tenant isolation under real RLS ────────────────────────────

func rlsSession(t *testing.T, db *testdb.DB, tenant string, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.App.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		t.Fatal(err)
	}
	fn(tx)
}

func TestGap2_CrossTenant_ReadWithdrawInvalidateAndUpdate_AreBlocked(t *testing.T) {
	s, db := newStore(t)
	nvA := publishedNotice(t, s, tenantA)
	rA := record(t, s, tenantA, creq("shared-subject", "p1", nvA, "GRANTED"))
	if _, err := s.SetPreference(tctx(tenantA), tenantA, domain.SetPreferenceRequest{SubjectRef: "shared-subject", ChannelOrPurpose: "email", Value: "ENABLED", Source: "UI"}); err != nil {
		t.Fatal(err)
	}
	ctxB := tctx(tenantB)

	// READ
	res, err := s.ResolveConsentStatus(ctxB, "shared-subject", "p1")
	if err != nil || res.Status != domain.ConsentStatusNotRequested || res.LatestReceipt != nil {
		t.Fatalf("tenant B resolved tenant A's consent: %+v %v", res, err)
	}
	if _, err := s.FindConsentReceipt(ctxB, rA.ConsentReceiptID); !errors.Is(err, domain.ErrConsentReceiptNotFound) {
		t.Fatalf("tenant B read tenant A's receipt: %v", err)
	}
	if p, _ := s.ResolvePreference(ctxB, "shared-subject", "email"); p.Value != domain.PreferenceNotApplicable {
		t.Fatalf("tenant B read tenant A's preference: %+v", p)
	}
	// WITHDRAW / INVALIDATE
	if _, err := s.WithdrawConsent(ctxB, rA.ConsentReceiptID, "X", "b", "c"); !errors.Is(err, domain.ErrConsentReceiptNotFound) {
		t.Fatalf("tenant B withdrew tenant A's consent: %v", err)
	}
	if _, err := s.InvalidateConsent(ctxB, rA.ConsentReceiptID, "r", "X", "b", "c"); !errors.Is(err, domain.ErrConsentReceiptNotFound) {
		t.Fatalf("tenant B invalidated tenant A's consent: %v", err)
	}
	if n := count(t, db, "withdrawal_receipts") + count(t, db, "consent_invalidations"); n != 0 {
		t.Fatalf("cross-tenant action left %d evidence row(s)", n)
	}
	// BINDING: B cannot bind its own receipt or presentation to A's notice version.
	if _, err := s.RecordConsent(ctxB, tenantB, creq("sb", "p1", nvA, "GRANTED"), "b", "c"); !errors.Is(err, domain.ErrNoticeVersionInvalid) {
		t.Fatalf("tenant B bound a receipt to tenant A's notice: %v", err)
	}
	// And A's state is unchanged.
	if got := statusFor(t, s, tenantA, "shared-subject", "p1"); got != domain.ConsentStatusGranted {
		t.Fatalf("tenant A's consent was disturbed: %s", got)
	}

	// UPDATE / DELETE / forged INSERT straight at the tables.
	rlsSession(t, db, tenantB, func(tx pgx.Tx) {
		ctx := context.Background()
		for _, q := range []string{
			`UPDATE notices SET created_by_principal_id='hacked'`,
			`UPDATE preference_assertions SET source='hacked'`,
			`DELETE FROM notices`,
			`DELETE FROM preference_assertions`,
		} {
			tag, err := tx.Exec(ctx, q)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("tenant B changed %d row(s) of tenant A: %s", tag.RowsAffected(), q)
			}
		}
	})
	rlsSession(t, db, tenantB, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `INSERT INTO notices (notice_id, tenant_id, created_by_principal_id) VALUES (gen_random_uuid(), $1, 'x')`, tenantA); err == nil {
			t.Errorf("tenant B inserted a notice owned by tenant A")
		}
	})
}

func TestGap2_NullTenantRows_AreInvisibleToEveryTenant_AndCannotBeActedOn(t *testing.T) {
	s, db := newStore(t)
	ctx := context.Background()
	nv := publishedNotice(t, s, tenantA)

	// New records cannot bypass tenant assignment — not even for a superuser.
	_, err := db.Admin.Exec(ctx, `INSERT INTO consent_receipts (consent_receipt_id, tenant_id, subject_ref, purpose_id, action, capture_channel, actor_principal_id)
		VALUES (gen_random_uuid(), NULL, 'x', 'p', 'GRANTED', 'WEB', 'a')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("a NULL-tenant consent receipt must violate the tenant-assigned CHECK (23514), got %v", err)
	}
	rlsSession(t, db, tenantA, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `INSERT INTO consent_receipts (consent_receipt_id, tenant_id, subject_ref, purpose_id, action, capture_channel, actor_principal_id)
			VALUES (gen_random_uuid(), NULL, 'x', 'p', 'GRANTED', 'WEB', 'a')`); err == nil {
			t.Errorf("the application role inserted a NULL-tenant row")
		}
	})

	// A LEGACY NULL-tenant row (from before the constraint) is simulated by dropping it.
	if _, err := db.Admin.Exec(ctx, `ALTER TABLE consent_receipts DROP CONSTRAINT consent_receipts_tenant_assigned`); err != nil {
		t.Fatal(err)
	}
	var legacyID string
	if err := db.Admin.QueryRow(ctx, `INSERT INTO consent_receipts (consent_receipt_id, tenant_id, subject_ref, purpose_id, action, capture_channel, actor_principal_id)
		VALUES (gen_random_uuid(), NULL, 'legacy-subject', 'p1', 'GRANTED', 'WEB', 'a') RETURNING consent_receipt_id::text`).Scan(&legacyID); err != nil {
		t.Fatal(err)
	}

	for _, tenant := range []string{tenantA, tenantB, ""} {
		rlsSession(t, db, tenant, func(tx pgx.Tx) {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM consent_receipts WHERE subject_ref='legacy-subject'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("tenant %q can see a NULL-tenant receipt (%d)", tenant, n)
			}
		})
	}
	for _, c := range []context.Context{tctx(tenantA), tctx(tenantB), context.Background()} {
		if _, err := s.FindConsentReceipt(c, legacyID); !errors.Is(err, domain.ErrConsentReceiptNotFound) {
			t.Errorf("a NULL-tenant receipt was readable: %v", err)
		}
		if _, err := s.WithdrawConsent(c, legacyID, "X", "u", "c"); !errors.Is(err, domain.ErrConsentReceiptNotFound) {
			t.Errorf("a NULL-tenant receipt could be withdrawn: %v", err)
		}
		if _, err := s.InvalidateConsent(c, legacyID, "r", "X", "u", "c"); !errors.Is(err, domain.ErrConsentReceiptNotFound) {
			t.Errorf("a NULL-tenant receipt could be invalidated: %v", err)
		}
	}
	if res, _ := s.ResolveConsentStatus(tctx(tenantA), "legacy-subject", "p1"); res.Status != domain.ConsentStatusNotRequested {
		t.Errorf("legacy NULL-tenant consent leaked into tenant A's derived status: %s", res.Status)
	}
	if n := count(t, db, "withdrawal_receipts") + count(t, db, "consent_invalidations"); n != 0 {
		t.Errorf("a NULL-tenant receipt was acted on")
	}
	// Nothing was written for a tenant-less context either.
	if _, err := s.RecordConsent(context.Background(), tenantA, creq("nt", "p1", nv, "GRANTED"), "a", "c"); err == nil {
		t.Errorf("a context with no tenant recorded consent")
	}
}

func TestGap2_PolicyStructure_NoNullTenantException_AndForcedRLS(t *testing.T) {
	_, db := newStore(t)
	ctx := context.Background()
	tables := []string{"notices", "notice_versions", "presentation_receipts", "consent_receipts", "withdrawal_receipts",
		"preference_assertions", "consent_requests", "consent_invalidations", "consent_idempotency_keys", "event_outbox"}

	for _, tbl := range tables {
		var enabled, forced bool
		if err := db.Admin.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, tbl).Scan(&enabled, &forced); err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced {
			t.Errorf("%s: RLS enabled=%v forced=%v — both must be true", tbl, enabled, forced)
		}
		rows, err := db.Admin.Query(ctx, `SELECT policyname, COALESCE(qual,''), COALESCE(with_check,'') FROM pg_policies WHERE schemaname='public' AND tablename=$1`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var name, qual, check string
			_ = rows.Scan(&name, &qual, &check)
			n++
			for _, expr := range []string{qual, check} {
				if strings.Contains(strings.ToUpper(expr), "IS NULL") {
					t.Errorf("%s policy %s still contains a NULL-tenant exception: %s", tbl, name, expr)
				}
			}
		}
		rows.Close()
		if n == 0 {
			t.Errorf("%s has no RLS policy", tbl)
		}
	}
}

func TestGap2_Outbox_IsNotReadableOrWritableAcrossTenants(t *testing.T) {
	s, db := newStore(t)
	nv := publishedNotice(t, s, tenantA)
	record(t, s, tenantA, creq("s-out", "p1", nv, "GRANTED"))

	cnt := func(tenant string) int {
		var n int
		rlsSession(t, db, tenant, func(tx pgx.Tx) {
			if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM event_outbox WHERE entity_id='s-out'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
		})
		return n
	}
	if cnt(tenantA) != 1 {
		t.Errorf("the owning tenant should see its own pending event")
	}
	if n := cnt(tenantB); n != 0 {
		t.Errorf("tenant B can read tenant A's pending events (%d)", n)
	}
	if n := cnt(""); n != 0 {
		t.Errorf("a session with no tenant can read pending events (%d)", n)
	}
	rlsSession(t, db, "", func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `INSERT INTO event_outbox (outbox_event_id,event_type,tenant_id,payload) VALUES ('evt-x','x','',$1)`, `{}`); err == nil {
			t.Errorf("a tenant-less session injected an event")
		}
	})
}
