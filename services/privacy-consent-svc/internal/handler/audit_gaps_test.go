package handler_test

// Regression tests for the gaps found by the 64.7% documentation-compliance
// audit of privacy-consent-svc. Each test names the audit gap it pins. The
// store-level behaviour of the same fixes is proven against real PostgreSQL
// in internal/store/audit_gaps_integration_test.go.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/privacy-consent-svc/internal/domain"
)

func grantBody(subject string) domain.RecordConsentRequest {
	return domain.RecordConsentRequest{
		SubjectRef: subject, PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
		NoticeVersionID: testPublishedNoticeVersionID,
	}
}

func decodeMap(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, w.Body.String())
	}
	return m
}

func errorCodeOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	m := decodeMap(t, w)
	code, _ := m["error_code"].(string)
	return code
}

func statusOf(t *testing.T, r http.Handler, subject string) domain.ConsentStatus {
	t.Helper()
	w := doRequest(r, http.MethodGet, "/privacy/consents?subject_ref="+subject+"&purpose_id="+testPurpose, nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("status read: HTTP %d %s", w.Code, w.Body.String())
	}
	var res domain.ConsentResolution
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return res.Status
}

func mustGrant(t *testing.T, r http.Handler, body domain.RecordConsentRequest) domain.ConsentReceipt {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/consents", body, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("grant: HTTP %d %s", w.Code, w.Body.String())
	}
	var c domain.ConsentReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	return c
}

// ── GAP 1: re-grant after INVALIDATED / EXPIRED ──────────────────────────────

func TestGap1_ReplayOfActiveConsent_ReturnsTheOriginalReceipt(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	first := mustGrant(t, r, grantBody("s-replay"))
	second := mustGrant(t, r, grantBody("s-replay"))
	if first.ConsentReceiptID != second.ConsentReceiptID {
		t.Fatalf("PRV-N04: an active consent replayed must return the original receipt (%s vs %s)", first.ConsentReceiptID, second.ConsentReceiptID)
	}
	if len(st.consentReceipts) != 1 || len(st.outboxEvents) != 1 {
		t.Fatalf("a replay must create no evidence and no event: receipts=%d events=%d", len(st.consentReceipts), len(st.outboxEvents))
	}
}

func TestGap1_NewConsentAfterInvalidation_CreatesANewGrantedReceipt(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	first := mustGrant(t, r, grantBody("s-inv"))

	w := doRequest(r, http.MethodPost, "/privacy/consents/"+first.ConsentReceiptID+"/invalidate",
		domain.InvalidateConsentRequest{Reason: "capture channel compromised", Channel: "SECURITY_OPS"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("invalidate: %d %s", w.Code, w.Body.String())
	}
	if got := statusOf(t, r, "s-inv"); got != domain.ConsentStatusInvalidated {
		t.Fatalf("precondition: want INVALIDATED, got %s", got)
	}

	second := mustGrant(t, r, grantBody("s-inv"))
	if second.ConsentReceiptID == first.ConsentReceiptID {
		t.Fatalf("the dead INVALIDATED receipt was returned instead of a new one — the subject can never consent again")
	}
	if got := statusOf(t, r, "s-inv"); got != domain.ConsentStatusGranted {
		t.Fatalf("the new receipt must become GRANTED, got %s", got)
	}
	// Old evidence is preserved, and still invalidated.
	if _, ok := st.consentReceipts[first.ConsentReceiptID]; !ok {
		t.Fatalf("the original receipt was removed")
	}
	if _, ok := st.invalidations[first.ConsentReceiptID]; !ok {
		t.Fatalf("the original invalidation record was lost")
	}
	if len(st.consentReceipts) != 2 {
		t.Fatalf("want both receipts retained, have %d", len(st.consentReceipts))
	}
}

func TestGap1_NewConsentAfterExpiry_CreatesANewGrantedReceipt(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	past := time.Now().Add(-time.Hour).UTC()
	body := grantBody("s-exp")
	body.ExpiresAt = &past
	first := mustGrant(t, r, body)
	if got := statusOf(t, r, "s-exp"); got != domain.ConsentStatusExpired {
		t.Fatalf("precondition: want EXPIRED, got %s", got)
	}

	renewed := grantBody("s-exp")
	future := time.Now().Add(24 * time.Hour).UTC()
	renewed.ExpiresAt = &future
	second := mustGrant(t, r, renewed)
	if second.ConsentReceiptID == first.ConsentReceiptID {
		t.Fatalf("the EXPIRED receipt was returned instead of a new one")
	}
	if got := statusOf(t, r, "s-exp"); got != domain.ConsentStatusGranted {
		t.Fatalf("the renewed receipt must be GRANTED, got %s", got)
	}
	if _, ok := st.consentReceipts[first.ConsentReceiptID]; !ok {
		t.Fatalf("the expired receipt's evidence was removed")
	}
}

func TestGap1_NewConsentAfterWithdrawalAndAfterALaterDeny(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	g1 := mustGrant(t, r, grantBody("s-seq"))
	doRequest(r, http.MethodPost, "/privacy/consents/"+g1.ConsentReceiptID+"/withdraw", domain.WithdrawConsentRequest{Channel: "ACCOUNT"}, testTenant)
	g2 := mustGrant(t, r, grantBody("s-seq"))
	if g2.ConsentReceiptID == g1.ConsentReceiptID {
		t.Fatalf("a grant after a withdrawal must be a new receipt")
	}
	// GRANT, then DENY, then GRANT again: the old grant is no longer the live
	// outcome (the later DENY is), so the second GRANT must not be answered with it.
	deny := grantBody("s-seq")
	deny.Action = "DENIED"
	mustGrant(t, r, deny)
	g3 := mustGrant(t, r, grantBody("s-seq"))
	if g3.ConsentReceiptID == g2.ConsentReceiptID {
		t.Fatalf("a stale earlier GRANT answered a request made after a later DENY")
	}
	if got := statusOf(t, r, "s-seq"); got != domain.ConsentStatusGranted {
		t.Fatalf("want GRANTED, got %s", got)
	}
}

// ── GAP 4: offline / imported consent provenance (§11.1) ─────────────────────

func offlineBody() domain.RecordConsentRequest {
	imported := time.Now().Add(-5 * 365 * 24 * time.Hour).UTC()
	b := grantBody("s-offline")
	b.IsOfflineImport = true
	b.ImportSource = "legacy-crm"
	b.ImportCaptureMethod = "paper-form"
	b.ImportEvidenceRef = "scan-0042"
	b.ImportedAt = &imported
	return b
}

func TestGap4_OfflineImport_RequiresEveryProvenanceField(t *testing.T) {
	cases := map[string]func(*domain.RecordConsentRequest){
		"source":                func(b *domain.RecordConsentRequest) { b.ImportSource = "" },
		"capture method":        func(b *domain.RecordConsentRequest) { b.ImportCaptureMethod = "" },
		"original evidence ref": func(b *domain.RecordConsentRequest) { b.ImportEvidenceRef = "" },
		"migration provenance (imported_at)": func(b *domain.RecordConsentRequest) { b.ImportedAt = nil },
		"whitespace-only source":             func(b *domain.RecordConsentRequest) { b.ImportSource = "   " },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			st := newStubStore()
			r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
			b := offlineBody()
			mutate(&b)
			w := doRequest(r, http.MethodPost, "/privacy/consents", b, testTenant)
			if w.Code != http.StatusBadRequest || errorCodeOf(t, w) != domain.ErrCodeImportProvenanceRequired {
				t.Fatalf("want 400 %s, got %d %s", domain.ErrCodeImportProvenanceRequired, w.Code, w.Body.String())
			}
			if len(st.consentReceipts) != 0 {
				t.Fatalf("an incomplete offline import was stored")
			}
		})
	}
}

func TestGap4_OfflineImport_NamesAllMissingFields(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	b := grantBody("s-offline")
	b.IsOfflineImport = true
	w := doRequest(r, http.MethodPost, "/privacy/consents", b, testTenant)
	msg := decodeMap(t, w)["error"].(string)
	for _, f := range []string{"import_source", "import_capture_method", "import_evidence_ref", "imported_at"} {
		if !strings.Contains(msg, f) {
			t.Errorf("the error should name %s: %q", f, msg)
		}
	}
}

func TestGap4_OfflineImport_FutureImportedAtIsRefused(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	b := offlineBody()
	future := time.Now().Add(48 * time.Hour).UTC()
	b.ImportedAt = &future
	w := doRequest(r, http.MethodPost, "/privacy/consents", b, testTenant)
	if w.Code != http.StatusBadRequest || errorCodeOf(t, w) != domain.ErrCodeImportProvenanceRequired {
		t.Fatalf("a consent cannot have happened in the future: %d %s", w.Code, w.Body.String())
	}
}

func TestGap4_CompleteOfflineImport_IsAccepted_AndOnlineConsentIsUnaffected(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	c := mustGrant(t, r, offlineBody())
	if !c.IsOfflineImport || c.ImportSource == nil || *c.ImportSource != "legacy-crm" || c.ImportedAt == nil {
		t.Fatalf("complete offline import not preserved: %+v", c)
	}
	// Online consent needs none of it.
	online := mustGrant(t, r, grantBody("s-online"))
	if online.IsOfflineImport {
		t.Fatalf("online consent was flagged as an import")
	}
}

// ── GAP 6: §18 response schema ───────────────────────────────────────────────

func TestGap6_ConsentResponse_CarriesSection18Fields(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	b := grantBody("s-18")
	b.ConsentRequest = &domain.ConsentRequest{PurposeID: testPurpose, Scope: "marketing-email"}
	w := doRequest(r, http.MethodPost, "/privacy/consents", b, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	m := decodeMap(t, w)

	// §18: receipt_id, purpose/scope, notice_version, status, evidence_hash
	for field, want := range map[string]any{
		"purpose_id": testPurpose, "scope": "marketing-email",
		"notice_version": testPublishedNoticeVersionID, "status": "GRANTED",
	} {
		if m[field] != want {
			t.Errorf("%s = %v, want %v", field, m[field], want)
		}
	}
	id, _ := m["receipt_id"].(string)
	hash, _ := m["evidence_hash"].(string)
	if id == "" || !strings.HasPrefix(hash, domain.ReceiptIntegrityPrefix) {
		t.Errorf("receipt_id=%q evidence_hash=%q", id, hash)
	}
	// The names consumers already use carry the same values.
	if m["consent_receipt_id"] != id || m["receipt_integrity_hash"] != hash || m["action"] != "GRANTED" {
		t.Errorf("existing field names no longer match the §18 aliases: %v", m)
	}
}

func TestGap6_DeniedConsent_ReportsStatusDENIED(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	b := grantBody("s-denied")
	b.Action = "DENIED"
	w := doRequest(r, http.MethodPost, "/privacy/consents", b, testTenant)
	if m := decodeMap(t, w); m["status"] != "DENIED" {
		t.Fatalf("status = %v", m["status"])
	}
}

func TestGap6_WithdrawalResponse_CarriesSection18Fields(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	c := mustGrant(t, r, grantBody("s-w18"))
	w := doRequest(r, http.MethodPost, "/privacy/consents/"+c.ConsentReceiptID+"/withdraw", domain.WithdrawConsentRequest{Channel: "ACCOUNT"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	m := decodeMap(t, w)
	for _, f := range []string{"withdrawal_id", "effective_at", "propagation_correlation"} {
		if s, _ := m[f].(string); s == "" {
			t.Errorf("§18 field %s missing: %v", f, m)
		}
	}
	if m["withdrawal_id"] != m["withdrawal_receipt_id"] || m["effective_at"] != m["created_at"] || m["propagation_correlation"] != m["propagation_correlation_id"] {
		t.Errorf("§18 names and existing names disagree: %v", m)
	}
	if !strings.HasPrefix(m["propagation_correlation"].(string), "prop-") {
		t.Errorf("propagation_correlation = %v", m["propagation_correlation"])
	}
}

func TestGap6_ResolutionEmbedsTheSameShape(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	mustGrant(t, r, grantBody("s-res"))
	w := doRequest(r, http.MethodGet, "/privacy/consents?subject_ref=s-res&purpose_id="+testPurpose, nil, testTenant)
	latest, _ := decodeMap(t, w)["latest_receipt"].(map[string]any)
	if latest["receipt_id"] == nil || latest["evidence_hash"] == nil || latest["status"] != "GRANTED" {
		t.Fatalf("latest_receipt lacks the §18 fields: %v", latest)
	}
}

// ── GAP 7: idempotency fails safe ────────────────────────────────────────────

func TestGap7_Replay_ReturnsTheOriginalResponse(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	h := map[string]string{"Idempotency-Key": "k-replay"}
	w1 := doRequestWithHeaders(r, http.MethodPost, "/privacy/preferences", domain.SetPreferenceRequest{SubjectRef: "s", ChannelOrPurpose: "email", Value: "ENABLED", Source: "UI"}, testTenant, "p1", h)
	w2 := doRequestWithHeaders(r, http.MethodPost, "/privacy/preferences", domain.SetPreferenceRequest{SubjectRef: "s", ChannelOrPurpose: "email", Value: "ENABLED", Source: "UI"}, testTenant, "p1", h)
	if w1.Code != http.StatusCreated || w2.Code != http.StatusCreated || w2.Header().Get("Idempotency-Replay") != "true" {
		t.Fatalf("replay not served: %d/%d replay=%q", w1.Code, w2.Code, w2.Header().Get("Idempotency-Replay"))
	}
	if w1.Body.String() != w2.Body.String() {
		t.Fatalf("replay body differs")
	}
	if len(st.preferences) != 1 {
		t.Fatalf("replay created a second preference (%d)", len(st.preferences))
	}
}

func TestGap7_MismatchedRequest_Returns409(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	h := map[string]string{"Idempotency-Key": "k-mismatch"}
	doRequestWithHeaders(r, http.MethodPost, "/privacy/preferences", domain.SetPreferenceRequest{SubjectRef: "s", ChannelOrPurpose: "email", Value: "ENABLED", Source: "UI"}, testTenant, "p1", h)
	w := doRequestWithHeaders(r, http.MethodPost, "/privacy/preferences", domain.SetPreferenceRequest{SubjectRef: "s", ChannelOrPurpose: "email", Value: "DISABLED", Source: "UI"}, testTenant, "p1", h)
	if w.Code != http.StatusConflict || errorCodeOf(t, w) != domain.ErrCodeIdempotencyMismatch {
		t.Fatalf("want 409 %s, got %d %s", domain.ErrCodeIdempotencyMismatch, w.Code, w.Body.String())
	}
}

func TestGap7_LookupFailure_FailsClosed_AndExecutesNothing(t *testing.T) {
	st := newStubStore()
	st.idempotencyGetErr = errors.New("db down")
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	w := doRequest(r, http.MethodPost, "/privacy/consents", grantBody("s-lookup"), testTenant)
	if w.Code != http.StatusServiceUnavailable || errorCodeOf(t, w) != domain.ErrCodeStoreUnavailable {
		t.Fatalf("want 503 %s, got %d %s", domain.ErrCodeStoreUnavailable, w.Code, w.Body.String())
	}
	if len(st.consentReceipts) != 0 {
		t.Fatalf("a request ran without replay protection (%d receipts)", len(st.consentReceipts))
	}
}

func TestGap7_SaveFailure_IsReported_NotIgnored(t *testing.T) {
	st := newStubStore()
	st.idempotencySaveErr = errors.New("db down")
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	w := doRequest(r, http.MethodPost, "/privacy/consents", grantBody("s-save"), testTenant)
	if w.Code != http.StatusInternalServerError || errorCodeOf(t, w) != domain.ErrCodeIdempotencyNotRecorded {
		t.Fatalf("want 500 %s, got %d %s", domain.ErrCodeIdempotencyNotRecorded, w.Code, w.Body.String())
	}
	m := decodeMap(t, w)
	if m["committed"] != true {
		t.Fatalf("the response must say the operation committed: %v", m)
	}
	result, _ := m["result"].(map[string]any)
	if result["receipt_id"] == nil || len(st.consentReceipts) != 1 {
		t.Fatalf("the committed result must be returned so the caller need not repeat the operation: %v", m)
	}
}

func TestGap7_MissingKey_StillRejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	w := doRequestWithHeaders(r, http.MethodPost, "/privacy/consents", grantBody("s"), testTenant, "p1", map[string]string{"Idempotency-Key": ""})
	if w.Code != http.StatusBadRequest || errorCodeOf(t, w) != domain.ErrCodeIdempotencyRequired {
		t.Fatalf("the mandatory key was weakened: %d %s", w.Code, w.Body.String())
	}
}

// ── GAP 9: canonical latest-version routes authorize before they look up ─────

func latestRoutes(noticeID string) []string {
	base := "/privacy/notices/" + noticeID
	return []string{base + "/approve", base + "/publish", base + "/withdraw"}
}

func TestGap9_Unauthorized_FailsBeforeAnyResourceLookup(t *testing.T) {
	st := newStubStore()
	az := &stubAuthz{deny: true}
	r := newTestRouter(st, az, grantingPurposeRegistry())

	for _, noticeID := range []string{testPublishedNoticeID, uuid.NewString()} { // existing and nonexistent
		for _, path := range latestRoutes(noticeID) {
			w := doRequest(r, http.MethodPost, path, nil, testTenant)
			if w.Code != http.StatusForbidden || errorCodeOf(t, w) != domain.ErrCodeAuthorizationDenied {
				t.Fatalf("%s: want 403, got %d %s", path, w.Code, w.Body.String())
			}
		}
	}
	if st.findLatestCalls != 0 {
		t.Fatalf("the notice was looked up %d time(s) before the caller was authorized — existence can be probed", st.findLatestCalls)
	}
}

func TestGap9_ExistingAndNonexistentNotices_AnswerIdenticallyToAnUnauthorizedCaller(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{deny: true}, grantingPurposeRegistry())
	a := doRequest(r, http.MethodPost, "/privacy/notices/"+testPublishedNoticeID+"/approve", nil, testTenant)
	b := doRequest(r, http.MethodPost, "/privacy/notices/"+uuid.NewString()+"/approve", nil, testTenant)
	if a.Code != b.Code || a.Body.String() != b.Body.String() {
		t.Fatalf("responses differ and leak existence:\n%d %s\n%d %s", a.Code, a.Body.String(), b.Code, b.Body.String())
	}
}

func TestGap9_ChecksRunInOrder_PrincipalTenantContextAuthorization(t *testing.T) {
	st := newStubStore()
	az := &stubAuthz{}
	r := newTestRouter(st, az, grantingPurposeRegistry())
	path := "/privacy/notices/" + uuid.NewString() + "/approve"

	// 1. principal
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("X-Correlation-ID", "c")
	req.Header.Set("Idempotency-Key", "k")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || errorCodeOf(t, w) != domain.ErrCodeAuthenticationFailed {
		t.Fatalf("principal first: %d %s", w.Code, w.Body.String())
	}
	// 2. tenant
	if w := doRequest(r, http.MethodPost, path, nil, ""); w.Code != http.StatusUnauthorized || errorCodeOf(t, w) != domain.ErrCodeTenantScopeRequired {
		t.Fatalf("tenant second: %d %s", w.Code, w.Body.String())
	}
	// 3. required request context (correlation) — before authorization
	az.deny = true
	if w := doRequestWithHeaders(r, http.MethodPost, path, nil, testTenant, "p1", map[string]string{"X-Correlation-ID": ""}); w.Code != http.StatusBadRequest || errorCodeOf(t, w) != domain.ErrCodeCorrelationRequired {
		t.Fatalf("correlation third: %d %s", w.Code, w.Body.String())
	}
	// 4. authorization — and still no lookup
	if w := doRequest(r, http.MethodPost, path, nil, testTenant); w.Code != http.StatusForbidden {
		t.Fatalf("authorization fourth: %d %s", w.Code, w.Body.String())
	}
	if st.findLatestCalls != 0 {
		t.Fatalf("lookup happened before authorization")
	}
	// 5. only now is the notice resolved: authorized + unknown notice -> 404
	az.deny = false
	if w := doRequest(r, http.MethodPost, path, nil, testTenant); w.Code != http.StatusNotFound {
		t.Fatalf("authorized request for an unknown notice must 404: %d %s", w.Code, w.Body.String())
	}
	if st.findLatestCalls != 1 {
		t.Fatalf("expected exactly one lookup after authorization, saw %d", st.findLatestCalls)
	}
}

func TestGap9_AuthorizedCanonicalRoutesStillWork(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	w := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices", domain.CreateNoticeRequest{Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:x"}, testTenant, "maker")
	v := decodeVersion(t, w)
	if w := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices/"+v.NoticeID+"/approve", nil, testTenant, "checker"); w.Code != http.StatusOK {
		t.Fatalf("approve (latest): %d %s", w.Code, w.Body.String())
	}
	if w := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices/"+v.NoticeID+"/publish", nil, testTenant, "publisher"); w.Code != http.StatusOK {
		t.Fatalf("publish (latest): %d %s", w.Code, w.Body.String())
	}
	if w := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices/"+v.NoticeID+"/withdraw", nil, testTenant, "publisher"); w.Code != http.StatusOK {
		t.Fatalf("withdraw (latest): %d %s", w.Code, w.Body.String())
	}
}

func decodeVersion(t *testing.T, w *httptest.ResponseRecorder) domain.NoticeVersion {
	t.Helper()
	var v domain.NoticeVersion
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.NoticeID == "" {
		t.Fatalf("decode notice version: %v %s", err, w.Body.String())
	}
	return v
}

// ── GAP 2 (handler half): cross-tenant withdrawal / invalidation ─────────────

func TestGap2_ForeignTenantReceipt_CannotBeWithdrawnOrInvalidated(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	c := mustGrant(t, r, grantBody("s-x"))
	// Tenant B authenticates fine and is authorized — and still cannot reach it.
	other := "tenant-b"
	for _, tc := range []struct {
		name, path string
		body       any
	}{
		{"withdraw", "/privacy/consents/" + c.ConsentReceiptID + "/withdraw", domain.WithdrawConsentRequest{Channel: "X"}},
		{"invalidate", "/privacy/consents/" + c.ConsentReceiptID + "/invalidate", domain.InvalidateConsentRequest{Reason: "r", Channel: "X"}},
	} {
		w := doRequest(r, http.MethodPost, tc.path, tc.body, other)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: tenant B reached tenant A's receipt: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	if len(st.withdrawalReceipts) != 0 || len(st.invalidations) != 0 {
		t.Fatalf("cross-tenant action left evidence behind")
	}
}

func TestGap2_ReceiptWithNoTenant_IsNotASharedScope(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	c := mustGrant(t, r, grantBody("s-null"))
	st.consentReceipts[c.ConsentReceiptID].TenantID = nil // a legacy NULL-tenant row, as the code sees it

	for _, path := range []string{
		"/privacy/consents/" + c.ConsentReceiptID + "/withdraw",
		"/privacy/consents/" + c.ConsentReceiptID + "/invalidate",
	} {
		var body any = domain.WithdrawConsentRequest{Channel: "X"}
		if strings.HasSuffix(path, "invalidate") {
			body = domain.InvalidateConsentRequest{Reason: "r", Channel: "X"}
		}
		if w := doRequest(r, http.MethodPost, path, body, testTenant); w.Code != http.StatusNotFound {
			t.Fatalf("%s: a NULL-tenant receipt must not be reachable from a tenant: %d %s", path, w.Code, w.Body.String())
		}
	}
	if len(st.withdrawalReceipts)+len(st.invalidations) != 0 {
		t.Fatalf("a NULL-tenant receipt was acted on")
	}
}

func TestGap2_Unauthorized_WithdrawAndInvalidate_FailBeforeTheReceiptLookup(t *testing.T) {
	st := newStubStore()
	az := &stubAuthz{deny: true}
	r := newTestRouter(st, az, grantingPurposeRegistry())
	for _, id := range []string{uuid.NewString(), testPublishedNoticeID} {
		a := doRequest(r, http.MethodPost, "/privacy/consents/"+id+"/withdraw", domain.WithdrawConsentRequest{Channel: "X"}, testTenant)
		b := doRequest(r, http.MethodPost, "/privacy/consents/"+id+"/invalidate", domain.InvalidateConsentRequest{Reason: "r", Channel: "X"}, testTenant)
		if a.Code != http.StatusForbidden || b.Code != http.StatusForbidden {
			t.Fatalf("want 403/403, got %d/%d", a.Code, b.Code)
		}
	}
	if st.findReceiptCalls != 0 {
		t.Fatalf("receipts were looked up for an unauthorized caller (%d)", st.findReceiptCalls)
	}
}

// ── GAP 11: decision-svc's consent lookup ────────────────────────────────────

// privacy-decision-svc calls GET /privacy/consents with X-Tenant-Id ONLY. This
// pins what that call receives today so the dependency cannot be forgotten or
// papered over: the read requires a principal, so it is refused. The fix
// belongs to the caller (it must present a service identity); the
// authorization requirement here must not be weakened to make it pass.
func TestGap11_DecisionServiceStyleLookup_WithoutAPrincipal_IsRefused(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	req := httptest.NewRequest(http.MethodGet, "/privacy/consents?subject_ref=s&purpose_id="+testPurpose, nil)
	req.Header.Set("X-Tenant-Id", testTenant) // exactly, and only, what decision-svc's client sends
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || errorCodeOf(t, w) != domain.ErrCodeAuthenticationFailed {
		t.Fatalf("expected the tenant-only lookup to be refused with 401, got %d %s", w.Code, w.Body.String())
	}
	// With a principal AND a grant the same read works.
	if w := doRequest(r, http.MethodGet, "/privacy/consents?subject_ref=s&purpose_id="+testPurpose, nil, testTenant); w.Code != http.StatusOK {
		t.Fatalf("authorized read failed: %d %s", w.Code, w.Body.String())
	}
}
