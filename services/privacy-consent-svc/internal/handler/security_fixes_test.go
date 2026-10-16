package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	authzpkg "zoiko.io/privacy-consent-svc/internal/authz"
	"zoiko.io/privacy-consent-svc/internal/domain"
	svcmiddleware "zoiko.io/privacy-consent-svc/internal/middleware"
)

// ── read authentication / authorization (gap: unauthenticated cross-tenant
// consent and preference reads) ─────────────────────────────────────────────

func TestGetConsentStatus_MissingPrincipal_Returns401(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequestWithHeaders(r, http.MethodGet, "/privacy/consents?subject_ref=subject-1&purpose_id="+testPurpose, nil, testTenant, "",
		map[string]string{"X-Principal-Id": ""})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("SECURITY VIOLATION: expected 401 with no principal on a read of subject consent state, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetConsentStatus_MissingTenant_Returns401(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequestWithHeaders(r, http.MethodGet, "/privacy/consents?subject_ref=subject-1&purpose_id="+testPurpose, nil, "", "principal-01", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("SECURITY VIOLATION: expected 401 with no verified tenant, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetConsentStatus_AuthorizationDenied_Returns403(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{deny: true}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodGet, "/privacy/consents?subject_ref=subject-1&purpose_id="+testPurpose, nil, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the caller holds no PRIVACY_CONSENT_READ grant, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetPreference_MissingPrincipal_Returns401(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequestWithHeaders(r, http.MethodGet, "/privacy/preferences?subject_ref=subject-1&channel_or_purpose=EMAIL_MARKETING", nil, testTenant, "",
		map[string]string{"X-Principal-Id": ""})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("SECURITY VIOLATION: expected 401 with no principal on a read of subject preference state, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetPreference_AuthorizationDenied_Returns403(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{deny: true}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodGet, "/privacy/preferences?subject_ref=subject-1&channel_or_purpose=EMAIL_MARKETING", nil, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the caller holds no PRIVACY_PREFERENCE_READ grant, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetNotice_MissingTenant_Returns401(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	notice := createPublishedNotice(t, r)

	w := doRequestWithHeaders(r, http.MethodGet, "/privacy/notices/"+notice.NoticeID, nil, "", "principal-01", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("SECURITY VIOLATION: expected 401 reading a notice with no verified tenant, got %d: %s", w.Code, w.Body.String())
	}
}

// ── mandatory Idempotency-Key / correlation ID (§18.1) ──────────────────────

func TestRecordConsent_MissingIdempotencyKey_Returns400(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequestWithHeaders(r, http.MethodPost, "/privacy/consents",
		domain.RecordConsentRequest{SubjectRef: "subject-1", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP", NoticeVersionID: testPublishedNoticeVersionID},
		testTenant, "principal-01", map[string]string{"Idempotency-Key": ""})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("§18.1 VIOLATION: expected 400 with no Idempotency-Key on a mutating API, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRecordConsent_MissingCorrelationID_Returns400(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequestWithHeaders(r, http.MethodPost, "/privacy/consents",
		domain.RecordConsentRequest{SubjectRef: "subject-1", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP", NoticeVersionID: testPublishedNoticeVersionID},
		testTenant, "principal-01", map[string]string{"X-Correlation-ID": ""})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("§18.1 VIOLATION: expected 400 with no X-Correlation-ID on a mutating API, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateNotice_MissingTenant_Returns401(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequestWithHeaders(r, http.MethodPost, "/privacy/notices",
		domain.CreateNoticeRequest{Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:abc"},
		"", "principal-01", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 creating a notice with no verified tenant (closes the implicit platform-wide-write gap), got %d: %s", w.Code, w.Body.String())
	}
}

// Every error response now carries a stable error_code, not just prose.
func TestErrorResponses_CarryStableErrorCode(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{deny: true}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/notices", domain.CreateNoticeRequest{
		Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:abc",
	}, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	var body struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.ErrorCode != domain.ErrCodeAuthorizationDenied {
		t.Fatalf("expected error_code=%s, got %q (body: %s)", domain.ErrCodeAuthorizationDenied, body.ErrorCode, w.Body.String())
	}
}

// ── platform-scope authorization no longer uses a hard-coded tenant ────────

// TestAuthzClient_SendsCallersVerifiedTenant_NeverHardcoded is the
// regression test for the authz/client.go fix: X-Tenant-Id sent to
// authorization-svc must be the caller's own verified tenant (from
// context), never the old hard-coded "11111111-1111-1111-1111-111111111111"
// sent for every platform-scope check regardless of which tenant the
// caller actually belonged to.
func TestAuthzClient_SendsCallersVerifiedTenant_NeverHardcoded(t *testing.T) {
	const hardcodedTenant = "11111111-1111-1111-1111-111111111111"
	const callerTenant = "tenant-real-caller"

	var gotTenantHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotTenantHeader = req.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	defer srv.Close()

	client := authzpkg.NewClient(srv.URL)
	ctx := context.Background()
	// Simulate the verified request context the way middleware.TenantContext does.
	ctx = svcmiddleware.WithTenant(ctx, callerTenant)

	// A PLATFORM-SCOPE check (legalEntityID == the platform sentinel) from a
	// caller who belongs to a real tenant. Before the fix, this always sent
	// the hard-coded tenant regardless of callerTenant.
	if err := client.CheckAllowed(ctx, "principal-1", "00000000-0000-0000-0000-00000000f001", "PRIVACY_NOTICE_CREATE"); err != nil {
		t.Fatalf("expected GRANTED, got error: %v", err)
	}
	if gotTenantHeader == hardcodedTenant {
		t.Fatalf("PLATFORM-SCOPE TENANT BUG: authorization-svc was asked about the hard-coded tenant %s instead of the caller's own verified tenant %s",
			hardcodedTenant, callerTenant)
	}
	if gotTenantHeader != callerTenant {
		t.Fatalf("expected X-Tenant-Id=%s (the caller's own verified tenant), got %q", callerTenant, gotTenantHeader)
	}
}

// ── ConsentRequest evidence persistence (gap: accepted then discarded) ─────

func TestRecordConsent_PersistsConsentRequestEvidence(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	cr := &domain.ConsentRequest{
		PurposeID: testPurpose, Scope: "marketing-emails", DataContext: "email_address",
		RecipientContext: "internal-crm", TechnologyContext: "cookie", Vendor: "Acme Analytics",
		StorageLifetime: "13 months", JurisdictionRuleRef: "EU-EPRIVACY-5.3", RequiredStatus: "OPTIONAL",
		PolicyPackage: "policy-v7",
	}
	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-cr", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
		NoticeVersionID: testPublishedNoticeVersionID,
		ConsentRequest:  cr,
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var receipt domain.ConsentReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)

	persisted, ok := st.consentRequests[receipt.ConsentReceiptID]
	if !ok {
		t.Fatalf("§10.1 VIOLATION: ConsentRequest evidence was accepted but never persisted for receipt %s", receipt.ConsentReceiptID)
	}
	if persisted.Vendor != "Acme Analytics" || persisted.StorageLifetime != "13 months" || persisted.JurisdictionRuleRef != "EU-EPRIVACY-5.3" {
		t.Fatalf("expected §11.1 tracking-technology fields (vendor/storage_lifetime/jurisdiction_rule_ref) preserved, got %+v", persisted)
	}
	if receipt.ConsentRequest == nil || receipt.ConsentRequest.Scope != "marketing-emails" {
		t.Fatalf("expected the response to echo the persisted ConsentRequest, got %+v", receipt.ConsentRequest)
	}
}

// ── withdrawal propagation correlation (gap: field did not exist) ──────────

func TestWithdrawConsent_GeneratesPropagationCorrelationID_StableOnReplay(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-prop", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
		NoticeVersionID: testPublishedNoticeVersionID,
	}, testTenant)
	var receipt domain.ConsentReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)

	w1 := doRequest(r, http.MethodPost, "/privacy/consents/"+receipt.ConsentReceiptID+"/withdraw",
		domain.WithdrawConsentRequest{Channel: "ACCOUNT_SETTINGS"}, testTenant)
	var withdrawal1 domain.WithdrawalReceipt
	_ = json.Unmarshal(w1.Body.Bytes(), &withdrawal1)
	if withdrawal1.PropagationCorrelationID == "" {
		t.Fatalf("§10.1 VIOLATION: expected a non-empty propagation_correlation_id on withdrawal")
	}

	w2 := doRequest(r, http.MethodPost, "/privacy/consents/"+receipt.ConsentReceiptID+"/withdraw",
		domain.WithdrawConsentRequest{Channel: "ACCOUNT_SETTINGS"}, testTenant)
	var withdrawal2 domain.WithdrawalReceipt
	_ = json.Unmarshal(w2.Body.Bytes(), &withdrawal2)
	if withdrawal2.PropagationCorrelationID != withdrawal1.PropagationCorrelationID {
		t.Fatalf("PRV-N05 VIOLATION: a replayed withdrawal must return the SAME propagation_correlation_id, got %s vs %s",
			withdrawal1.PropagationCorrelationID, withdrawal2.PropagationCorrelationID)
	}
}

// ── age-band / offline-import evidence (§11.1) ──────────────────────────────

func TestRecordConsent_PersistsAgeBandAndOfflineImportEvidence(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	importedAt := time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC)
	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-minor", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "PAPER_FORM_MIGRATION",
		NoticeVersionID:     testPublishedNoticeVersionID,
		AgeBand:             "13-15",
		IsOfflineImport:     true,
		ImportSource:        "legacy-crm-2019-export",
		ImportCaptureMethod: "WET_SIGNATURE",
		ImportEvidenceRef:   "scan-ref-8821",
		ImportedAt:          &importedAt,
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var receipt domain.ConsentReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)

	if receipt.AgeBand == nil || *receipt.AgeBand != "13-15" {
		t.Fatalf("§11.1 VIOLATION: expected age_band=13-15 preserved, got %v", receipt.AgeBand)
	}
	if !receipt.IsOfflineImport || receipt.ImportSource == nil || *receipt.ImportSource != "legacy-crm-2019-export" {
		t.Fatalf("§11.1 VIOLATION: expected offline-import provenance preserved, got is_offline_import=%v source=%v", receipt.IsOfflineImport, receipt.ImportSource)
	}
	if receipt.ImportedAt == nil || !receipt.ImportedAt.Equal(importedAt) {
		t.Fatalf("expected imported_at to be the real historical capture time (never fabricated as now), got %v", receipt.ImportedAt)
	}
	if receipt.CreatedAt.Equal(importedAt) {
		t.Fatalf("CreatedAt must be when this service recorded the row, not the historical import time")
	}
}

// ── EXPIRED and INVALIDATED lifecycle states (§6) ───────────────────────────

func TestResolveConsentStatus_ExpiredGrant(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	past := time.Now().UTC().Add(-time.Hour)
	doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-expired", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
		NoticeVersionID: testPublishedNoticeVersionID,
		ExpiresAt:       &past,
	}, testTenant)

	w := doRequest(r, http.MethodGet, "/privacy/consents?subject_ref=subject-expired&purpose_id="+testPurpose, nil, testTenant)
	var res domain.ConsentResolution
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Status != domain.ConsentStatusExpired {
		t.Fatalf("expected EXPIRED for a grant whose expires_at has passed, got %s", res.Status)
	}
}

func TestInvalidateConsent_PlatformInitiated_DistinctFromWithdrawal(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-inv", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
		NoticeVersionID: testPublishedNoticeVersionID,
	}, testTenant)
	var receipt domain.ConsentReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)

	wInv := doRequest(r, http.MethodPost, "/privacy/consents/"+receipt.ConsentReceiptID+"/invalidate",
		domain.InvalidateConsentRequest{Reason: "capture channel compromised — confirmed credential-stuffing incident", Channel: "SECURITY_OPS"}, testTenant)
	if wInv.Code != http.StatusOK {
		t.Fatalf("expected 200 invalidating a consent receipt, got %d: %s", wInv.Code, wInv.Body.String())
	}
	var inv domain.ConsentInvalidation
	_ = json.Unmarshal(wInv.Body.Bytes(), &inv)
	if inv.Reason == "" {
		t.Fatalf("expected a non-empty reason recorded on the invalidation")
	}

	wStatus := doRequest(r, http.MethodGet, "/privacy/consents?subject_ref=subject-inv&purpose_id="+testPurpose, nil, testTenant)
	var res domain.ConsentResolution
	_ = json.Unmarshal(wStatus.Body.Bytes(), &res)
	if res.Status != domain.ConsentStatusInvalidated {
		t.Fatalf("§6 VIOLATION: expected INVALIDATED (distinct from WITHDRAWN), got %s", res.Status)
	}
	if res.ConsentInvalidation == nil {
		t.Fatalf("expected the resolution to carry the ConsentInvalidation record")
	}

	// Idempotent replay: a second invalidation must not error or duplicate.
	wInv2 := doRequest(r, http.MethodPost, "/privacy/consents/"+receipt.ConsentReceiptID+"/invalidate",
		domain.InvalidateConsentRequest{Reason: "different reason text on replay", Channel: "SECURITY_OPS"}, testTenant)
	if wInv2.Code != http.StatusOK {
		t.Fatalf("expected 200 on replayed invalidation, got %d: %s", wInv2.Code, wInv2.Body.String())
	}
	var inv2 domain.ConsentInvalidation
	_ = json.Unmarshal(wInv2.Body.Bytes(), &inv2)
	if inv2.ConsentInvalidationID != inv.ConsentInvalidationID {
		t.Fatalf("expected the SAME invalidation record on replay, got %s vs %s", inv.ConsentInvalidationID, inv2.ConsentInvalidationID)
	}
}

func TestInvalidateConsent_RequiresReason(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-inv2", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
		NoticeVersionID: testPublishedNoticeVersionID,
	}, testTenant)
	var receipt domain.ConsentReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)

	wInv := doRequest(r, http.MethodPost, "/privacy/consents/"+receipt.ConsentReceiptID+"/invalidate",
		domain.InvalidateConsentRequest{Channel: "SECURITY_OPS"}, testTenant)
	if wInv.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no reason, got %d: %s", wInv.Code, wInv.Body.String())
	}
}

// ── Gap 1 & Gap 2 Helpers and Tests ──────────────────────────────────────────

func createNoticeInState(st *stubStore, tenant string, status domain.NoticeVersionStatus) (string, string) {
	now := time.Now().UTC()
	noticeID := uuid.NewString()
	versionID := uuid.NewString()
	st.notices[noticeID] = &domain.Notice{
		NoticeID:             noticeID,
		TenantID:             &tenant,
		CreatedByPrincipalID: "maker-1",
		CreatedAt:            now,
	}
	var eff *time.Time
	if status == domain.NoticeStatusPublished || status == domain.NoticeStatusWithdrawn || status == domain.NoticeStatusSuperseded {
		t := now.Add(-time.Hour)
		eff = &t
	}
	st.noticeVersions[versionID] = &domain.NoticeVersion{
		NoticeVersionID:      versionID,
		NoticeID:             noticeID,
		Locale:               "en-US",
		Audience:             "CUSTOMER",
		ContentHash:          "sha256:" + versionID,
		VersionStatus:        status,
		EffectiveFrom:        eff,
		CreatedByPrincipalID: "maker-1",
		CreatedAt:            now,
	}
	st.seq[versionID] = st.nextSequence(versionID)
	return noticeID, versionID
}

// ── Gap 1: RecordPresentation accepts ONLY PUBLISHED notice versions (PRV-008) ──

func TestRecordPresentation_RejectsDraftNoticeVersion_ReturnsPRV008(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	noticeID, versionID := createNoticeInState(st, testTenant, domain.NoticeStatusDraft)

	w := doRequest(r, http.MethodPost, "/privacy/notices/"+noticeID+"/versions/"+versionID+"/presentation-receipts",
		domain.RecordPresentationRequest{SubjectRef: "subject-pres", Channel: "WEB", Locale: "en-US"}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for DRAFT notice version presentation, got %d: %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.ErrorCode != domain.ErrCodeNoticeVersionInvalid {
		t.Fatalf("expected error_code=%s, got %s", domain.ErrCodeNoticeVersionInvalid, errResp.ErrorCode)
	}
}

func TestRecordPresentation_RejectsApprovedNoticeVersion_ReturnsPRV008(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	noticeID, versionID := createNoticeInState(st, testTenant, domain.NoticeStatusApproved)

	w := doRequest(r, http.MethodPost, "/privacy/notices/"+noticeID+"/versions/"+versionID+"/presentation-receipts",
		domain.RecordPresentationRequest{SubjectRef: "subject-pres", Channel: "WEB", Locale: "en-US"}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for APPROVED notice version presentation, got %d: %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.ErrorCode != domain.ErrCodeNoticeVersionInvalid {
		t.Fatalf("expected error_code=%s, got %s", domain.ErrCodeNoticeVersionInvalid, errResp.ErrorCode)
	}
}

func TestRecordPresentation_RejectsWithdrawnNoticeVersion_ReturnsPRV008(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	noticeID, versionID := createNoticeInState(st, testTenant, domain.NoticeStatusWithdrawn)

	w := doRequest(r, http.MethodPost, "/privacy/notices/"+noticeID+"/versions/"+versionID+"/presentation-receipts",
		domain.RecordPresentationRequest{SubjectRef: "subject-pres", Channel: "WEB", Locale: "en-US"}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for WITHDRAWN notice version presentation, got %d: %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.ErrorCode != domain.ErrCodeNoticeVersionInvalid {
		t.Fatalf("expected error_code=%s, got %s", domain.ErrCodeNoticeVersionInvalid, errResp.ErrorCode)
	}
}

func TestRecordPresentation_RejectsNonexistentNoticeVersion_ReturnsPRV008(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/notices/"+testPublishedNoticeID+"/versions/"+uuid.NewString()+"/presentation-receipts",
		domain.RecordPresentationRequest{SubjectRef: "subject-pres", Channel: "WEB", Locale: "en-US"}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for nonexistent notice version presentation, got %d: %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.ErrorCode != domain.ErrCodeNoticeVersionInvalid {
		t.Fatalf("expected error_code=%s, got %s", domain.ErrCodeNoticeVersionInvalid, errResp.ErrorCode)
	}
}

func TestRecordPresentation_CanonicalRoute_RejectsWhenNoPublishedVersion_ReturnsPRV008(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	noticeID, _ := createNoticeInState(st, testTenant, domain.NoticeStatusDraft)

	w := doRequest(r, http.MethodPost, "/privacy/notices/"+noticeID+"/presentation-receipts",
		domain.RecordPresentationRequest{SubjectRef: "subject-pres", Channel: "WEB", Locale: "en-US"}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for notice with no published version, got %d: %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.ErrorCode != domain.ErrCodeNoticeVersionInvalid {
		t.Fatalf("expected error_code=%s, got %s", domain.ErrCodeNoticeVersionInvalid, errResp.ErrorCode)
	}
}

func TestRecordPresentation_AcceptsPublishedNoticeVersion(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/notices/"+testPublishedNoticeID+"/versions/"+testPublishedNoticeVersionID+"/presentation-receipts",
		domain.RecordPresentationRequest{SubjectRef: "subject-pres", Channel: "WEB", Locale: "en-US"}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for PUBLISHED notice presentation, got %d: %s", w.Code, w.Body.String())
	}
}

// ── Gap 2: RecordConsent requires notice_version_id to exist and be PUBLISHED (PRV-008) ──

func TestRecordConsent_MissingNoticeVersionID_ReturnsPRV008(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-gap2", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
	}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when notice_version_id is missing, got %d: %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.ErrorCode != domain.ErrCodeNoticeVersionInvalid {
		t.Fatalf("expected error_code=%s, got %s", domain.ErrCodeNoticeVersionInvalid, errResp.ErrorCode)
	}
}

func TestRecordConsent_EmptyNoticeVersionID_ReturnsPRV008(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-gap2", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
		NoticeVersionID: "   ",
	}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when notice_version_id is whitespace, got %d: %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.ErrorCode != domain.ErrCodeNoticeVersionInvalid {
		t.Fatalf("expected error_code=%s, got %s", domain.ErrCodeNoticeVersionInvalid, errResp.ErrorCode)
	}
}

func TestRecordConsent_RejectsDraftNoticeVersion_ReturnsPRV008(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	_, versionID := createNoticeInState(st, testTenant, domain.NoticeStatusDraft)

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-gap2", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
		NoticeVersionID: versionID,
	}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when notice_version_id is DRAFT, got %d: %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.ErrorCode != domain.ErrCodeNoticeVersionInvalid {
		t.Fatalf("expected error_code=%s, got %s", domain.ErrCodeNoticeVersionInvalid, errResp.ErrorCode)
	}
}

func TestRecordConsent_RejectsApprovedNoticeVersion_ReturnsPRV008(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	_, versionID := createNoticeInState(st, testTenant, domain.NoticeStatusApproved)

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-gap2", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
		NoticeVersionID: versionID,
	}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when notice_version_id is APPROVED, got %d: %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.ErrorCode != domain.ErrCodeNoticeVersionInvalid {
		t.Fatalf("expected error_code=%s, got %s", domain.ErrCodeNoticeVersionInvalid, errResp.ErrorCode)
	}
}

func TestRecordConsent_RejectsWithdrawnNoticeVersion_ReturnsPRV008(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	_, versionID := createNoticeInState(st, testTenant, domain.NoticeStatusWithdrawn)

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-gap2", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
		NoticeVersionID: versionID,
	}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when notice_version_id is WITHDRAWN, got %d: %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.ErrorCode != domain.ErrCodeNoticeVersionInvalid {
		t.Fatalf("expected error_code=%s, got %s", domain.ErrCodeNoticeVersionInvalid, errResp.ErrorCode)
	}
}

func TestRecordConsent_RejectsNonexistentNoticeVersion_ReturnsPRV008(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-gap2", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
		NoticeVersionID: uuid.NewString(),
	}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when notice_version_id is nonexistent, got %d: %s", w.Code, w.Body.String())
	}
	var errResp struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.ErrorCode != domain.ErrCodeNoticeVersionInvalid {
		t.Fatalf("expected error_code=%s, got %s", domain.ErrCodeNoticeVersionInvalid, errResp.ErrorCode)
	}
}

func TestRecordConsent_AcceptsPublishedNoticeVersion(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-gap2", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
		NoticeVersionID: testPublishedNoticeVersionID,
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 when notice_version_id is PUBLISHED, got %d: %s", w.Code, w.Body.String())
	}
}
