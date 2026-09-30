package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-consent-svc/internal/authz"
	"zoiko.io/privacy-consent-svc/internal/domain"
	"zoiko.io/privacy-consent-svc/internal/events"
	"zoiko.io/privacy-consent-svc/internal/handler"
	"zoiko.io/privacy-consent-svc/internal/middleware"
)

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct {
	calls      int
	eventTypes []string
}

func (p *stubPublisher) Publish(_ context.Context, params events.PublishParams) error {
	p.calls++
	p.eventTypes = append(p.eventTypes, params.EventType)
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// ── stub authz ───────────────────────────────────────────────────────────────

type stubAuthz struct{ deny bool }

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

// ── stub purpose registry ────────────────────────────────────────────────────

// stubPurposeRegistry stands in for a real HTTP call to
// privacy-purpose-registry-svc — published tracks which purpose_ids
// resolve as currently published, exactly like the real PRV-01 service
// would answer for a purpose someone actually created and published there.
type stubPurposeRegistry struct {
	published map[string]bool
	err       error
}

func (p *stubPurposeRegistry) IsPublished(_ context.Context, purposeID string) (bool, error) {
	if p.err != nil {
		return false, p.err
	}
	return p.published[purposeID], nil
}

// ── test harness ─────────────────────────────────────────────────────────────

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz, purposes *stubPurposeRegistry) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, purposes, logger)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func doRequestWithHeaders(r http.Handler, method, path string, body interface{}, tenantID, principalID string, headers map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	} else {
		req.Header.Set("X-Principal-Id", "principal-01")
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func doRequestWithPrincipal(r http.Handler, method, path string, body interface{}, tenantID, principalID string) *httptest.ResponseRecorder {
	return doRequestWithHeaders(r, method, path, body, tenantID, principalID, nil)
}

func doRequest(r http.Handler, method, path string, body interface{}, tenantID string) *httptest.ResponseRecorder {
	return doRequestWithPrincipal(r, method, path, body, tenantID, "principal-01")
}

const testTenant = "tenant-consent-1"
const testPurpose = "purpose-marketing-emails"

func grantingPurposeRegistry() *stubPurposeRegistry {
	return &stubPurposeRegistry{published: map[string]bool{testPurpose: true}}
}

// ── notice lifecycle ─────────────────────────────────────────────────────────

func createPublishedNotice(t *testing.T, r http.Handler) *domain.NoticeVersion {
	t.Helper()
	w := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices", domain.CreateNoticeRequest{
		Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:abc123",
	}, testTenant, "principal-maker")
	var v domain.NoticeVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	base := "/privacy/notices/" + v.NoticeID + "/versions/" + v.NoticeVersionID
	wApp := doRequestWithPrincipal(r, http.MethodPost, base+"/approve", nil, testTenant, "principal-approver")
	if wApp.Code != http.StatusOK {
		t.Fatalf("expected 200 approving notice, got %d: %s", wApp.Code, wApp.Body.String())
	}
	wPub := doRequestWithPrincipal(r, http.MethodPost, base+"/publish", nil, testTenant, "principal-publisher")
	if wPub.Code != http.StatusOK {
		t.Fatalf("expected 200 publishing notice, got %d: %s", wPub.Code, wPub.Body.String())
	}
	var published domain.NoticeVersion
	_ = json.Unmarshal(wPub.Body.Bytes(), &published)
	return &published
}

func TestNoticeFullLifecycle(t *testing.T) {
	st := newStubStore()
	pub := &stubPublisher{}
	r := newTestRouter(st, pub, &stubAuthz{}, grantingPurposeRegistry())

	published := createPublishedNotice(t, r)
	if published.VersionStatus != domain.NoticeStatusPublished {
		t.Fatalf("expected PUBLISHED, got %s", published.VersionStatus)
	}
	if pub.eventTypes[len(pub.eventTypes)-1] != "privacy.notice.published" {
		t.Fatalf("expected privacy.notice.published event, got %v", pub.eventTypes)
	}

	base := "/privacy/notices/" + published.NoticeID + "/versions/" + published.NoticeVersionID
	w := doRequestWithPrincipal(r, http.MethodPost, base+"/withdraw", nil, testTenant, "principal-approver")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 withdrawing a PUBLISHED notice, got %d: %s", w.Code, w.Body.String())
	}
	var withdrawn domain.NoticeVersion
	_ = json.Unmarshal(w.Body.Bytes(), &withdrawn)
	if withdrawn.VersionStatus != domain.NoticeStatusWithdrawn {
		t.Fatalf("expected WITHDRAWN, got %s", withdrawn.VersionStatus)
	}
}

// TestPublishNotice_SupersedesPrior proves the side-effect PgStore.
// PublishNoticeVersion documents: publishing a successor demotes the
// previously PUBLISHED version to SUPERSEDED, never leaving two
// simultaneously PUBLISHED versions of the same notice.
func TestPublishNotice_SupersedesPrior(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	first := createPublishedNotice(t, r)

	wVersion := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices/"+first.NoticeID+"/versions", domain.CreateNoticeVersionRequest{
		ParentVersionID: first.NoticeVersionID, Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:def456",
	}, testTenant, "principal-maker-2")
	var second domain.NoticeVersion
	_ = json.Unmarshal(wVersion.Body.Bytes(), &second)

	base := "/privacy/notices/" + second.NoticeID + "/versions/" + second.NoticeVersionID
	wApp := doRequestWithPrincipal(r, http.MethodPost, base+"/approve", nil, testTenant, "principal-approver-2")
	if wApp.Code != http.StatusOK {
		t.Fatalf("expected 200 approving second notice, got %d: %s", wApp.Code, wApp.Body.String())
	}
	wPub := doRequestWithPrincipal(r, http.MethodPost, base+"/publish", nil, testTenant, "principal-publisher-2")
	if wPub.Code != http.StatusOK {
		t.Fatalf("expected 200 publishing second notice, got %d: %s", wPub.Code, wPub.Body.String())
	}

	wFirst := doRequest(r, http.MethodGet, "/privacy/notices/"+first.NoticeID+"?as_of=2099-01-01T00:00:00Z", nil, testTenant)
	var resolved domain.NoticeVersion
	_ = json.Unmarshal(wFirst.Body.Bytes(), &resolved)
	if resolved.NoticeVersionID != second.NoticeVersionID {
		t.Fatalf("expected the second (latest) version to resolve, got %s", resolved.NoticeVersionID)
	}

	// Directly confirm the first version was actually demoted, not just
	// that resolution picked the newer one.
	stub := st
	if stub.noticeVersions[first.NoticeVersionID].VersionStatus != domain.NoticeStatusSuperseded {
		t.Fatalf("FABRICATION: expected the first version to be SUPERSEDED, got %s", stub.noticeVersions[first.NoticeVersionID].VersionStatus)
	}
}

func TestPublishNotice_SkippingApproval_Rejected(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices", domain.CreateNoticeRequest{
		Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:abc123",
	}, testTenant, "principal-maker")
	var v domain.NoticeVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	wPub := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices/"+v.NoticeID+"/versions/"+v.NoticeVersionID+"/publish", nil, testTenant, "principal-publisher")
	if wPub.Code != http.StatusConflict {
		t.Fatalf("FABRICATION: expected 409 publishing a DRAFT (never approved) notice, got %d: %s", wPub.Code, wPub.Body.String())
	}
}

// ── presentation receipts ────────────────────────────────────────────────────

func TestRecordPresentation(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	notice := createPublishedNotice(t, r)
	session := "session-xyz"
	tmplVer := "v1.2"
	evidence := "signed_client_log"
	w := doRequest(r, http.MethodPost, "/privacy/notices/"+notice.NoticeID+"/versions/"+notice.NoticeVersionID+"/presentation-receipts",
		domain.RecordPresentationRequest{
			SubjectRef:       "subject-1",
			Channel:          "WEB_SIGNUP",
			Locale:           "en-US",
			SessionRef:       session,
			TemplateVersion:  tmplVer,
			DeliveryEvidence: evidence,
		}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var pr domain.PresentationReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &pr)
	if pr.SessionRef == nil || *pr.SessionRef != session {
		t.Fatalf("expected session_ref %s, got %v", session, pr.SessionRef)
	}
	if pr.TemplateVersion == nil || *pr.TemplateVersion != tmplVer {
		t.Fatalf("expected template_version %s, got %v", tmplVer, pr.TemplateVersion)
	}
	if pr.DeliveryEvidence == nil || *pr.DeliveryEvidence != evidence {
		t.Fatalf("expected delivery_evidence %s, got %v", evidence, pr.DeliveryEvidence)
	}
}

// ── consent ──────────────────────────────────────────────────────────────────

func TestRecordConsent_UnregisteredPurpose_Rejected(t *testing.T) {
	st := newStubStore()
	// Empty published map — nothing is registered.
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, &stubPurposeRegistry{published: map[string]bool{}})

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-1", PurposeID: "purpose-does-not-exist", Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
	}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("FABRICATION: expected 422 for an unregistered purpose, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRecordConsent_GrantThenResolve(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-1", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	wStatus := doRequest(r, http.MethodGet, "/privacy/consents?subject_ref=subject-1&purpose_id="+testPurpose, nil, testTenant)
	var res domain.ConsentResolution
	_ = json.Unmarshal(wStatus.Body.Bytes(), &res)
	if res.Status != domain.ConsentStatusGranted {
		t.Fatalf("expected GRANTED, got %s", res.Status)
	}
}

// TestWithdrawConsent_ThenResolveShowsWithdrawn_ButOriginalReceiptUntouched
// is the regression test for PRV-I09/I10/I11: withdrawal is a NEW fact,
// never a deletion or edit of the original grant.
// Also tests PRV-N05: replayed withdrawal must be idempotent and return original receipt.
func TestWithdrawConsent_ThenResolveShowsWithdrawn_ButOriginalReceiptUntouched(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-1", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
	}, testTenant)
	var receipt domain.ConsentReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)

	wWithdraw := doRequest(r, http.MethodPost, "/privacy/consents/"+receipt.ConsentReceiptID+"/withdraw",
		domain.WithdrawConsentRequest{Channel: "ACCOUNT_SETTINGS"}, testTenant)
	if wWithdraw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", wWithdraw.Code, wWithdraw.Body.String())
	}
	var firstWithdraw domain.WithdrawalReceipt
	_ = json.Unmarshal(wWithdraw.Body.Bytes(), &firstWithdraw)

	wStatus := doRequest(r, http.MethodGet, "/privacy/consents?subject_ref=subject-1&purpose_id="+testPurpose, nil, testTenant)
	var res domain.ConsentResolution
	_ = json.Unmarshal(wStatus.Body.Bytes(), &res)
	if res.Status != domain.ConsentStatusWithdrawn {
		t.Fatalf("expected WITHDRAWN, got %s", res.Status)
	}
	if res.LatestReceipt == nil || res.LatestReceipt.Action != domain.ConsentActionGranted {
		t.Fatalf("PRV-I10 VIOLATION: original GRANTED receipt must remain visible and untouched, got %+v", res.LatestReceipt)
	}

	// PRV-N05: Replayed withdrawal of the same receipt must return original effective withdrawal idempotently (200 OK)
	wSecond := doRequest(r, http.MethodPost, "/privacy/consents/"+receipt.ConsentReceiptID+"/withdraw",
		domain.WithdrawConsentRequest{Channel: "ACCOUNT_SETTINGS"}, testTenant)
	if wSecond.Code != http.StatusOK {
		t.Fatalf("PRV-N05 VIOLATION: expected 200 on replayed withdrawal, got %d: %s", wSecond.Code, wSecond.Body.String())
	}
	var secondWithdraw domain.WithdrawalReceipt
	_ = json.Unmarshal(wSecond.Body.Bytes(), &secondWithdraw)
	if secondWithdraw.WithdrawalReceiptID != firstWithdraw.WithdrawalReceiptID {
		t.Fatalf("PRV-N05 VIOLATION: expected identical withdrawal receipt ID on replay, got %s vs %s", secondWithdraw.WithdrawalReceiptID, firstWithdraw.WithdrawalReceiptID)
	}
}

func TestGetConsentStatus_NoReceipt_NotRequested(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodGet, "/privacy/consents?subject_ref=nobody&purpose_id="+testPurpose, nil, testTenant)
	var res domain.ConsentResolution
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Status != domain.ConsentStatusNotRequested {
		t.Fatalf("expected NOT_REQUESTED, got %s", res.Status)
	}
}

func TestRecordConsent_PurposeRegistryUnavailable_FailsClosed(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, &stubPurposeRegistry{err: context.DeadlineExceeded})

	w := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef: "subject-1", PurposeID: testPurpose, Action: "GRANTED", CaptureChannel: "WEB_SIGNUP",
	}, testTenant)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the purpose registry can't be reached, got %d: %s", w.Code, w.Body.String())
	}
}

// ── preferences ──────────────────────────────────────────────────────────────

func TestSetAndGetPreference(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/preferences", domain.SetPreferenceRequest{
		SubjectRef: "subject-1", ChannelOrPurpose: "EMAIL_MARKETING", Value: "DISABLED", Source: "ACCOUNT_SETTINGS",
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	wGet := doRequest(r, http.MethodGet, "/privacy/preferences?subject_ref=subject-1&channel_or_purpose=EMAIL_MARKETING", nil, testTenant)
	var p domain.PreferenceAssertion
	_ = json.Unmarshal(wGet.Body.Bytes(), &p)
	if p.Value != domain.PreferenceDisabled {
		t.Fatalf("expected DISABLED, got %s", p.Value)
	}
}

// TestPreference_NeverImpliesConsent is the regression test for PRV-I12:
// setting a preference must never create or affect a consent resolution.
func TestPreference_NeverImpliesConsent(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	doRequest(r, http.MethodPost, "/privacy/preferences", domain.SetPreferenceRequest{
		SubjectRef: "subject-1", ChannelOrPurpose: testPurpose, Value: "ENABLED", Source: "ACCOUNT_SETTINGS",
	}, testTenant)

	wStatus := doRequest(r, http.MethodGet, "/privacy/consents?subject_ref=subject-1&purpose_id="+testPurpose, nil, testTenant)
	var res domain.ConsentResolution
	_ = json.Unmarshal(wStatus.Body.Bytes(), &res)
	if res.Status != domain.ConsentStatusNotRequested {
		t.Fatalf("PRV-I12 VIOLATION: an ENABLED preference must not imply consent, expected NOT_REQUESTED, got %s", res.Status)
	}
}

func TestCreateNotice_AuthorizationDenied(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{deny: true}, grantingPurposeRegistry())

	w := doRequest(r, http.MethodPost, "/privacy/notices", domain.CreateNoticeRequest{
		Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:abc123",
	}, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// ── SoD: Maker-Checker Segregation of Duties (§7.1, §28) ─────────────────────

func TestNotice_MakerChecker_SoD_Enforcement(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	// Maker creates notice
	wCreate := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices", domain.CreateNoticeRequest{
		Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:hash1",
	}, testTenant, "principal-maker")
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating notice, got %d: %s", wCreate.Code, wCreate.Body.String())
	}
	var v domain.NoticeVersion
	_ = json.Unmarshal(wCreate.Body.Bytes(), &v)

	base := "/privacy/notices/" + v.NoticeID + "/versions/" + v.NoticeVersionID

	// 1. Maker attempts to approve own version -> MUST be rejected (403)
	wMakerApprove := doRequestWithPrincipal(r, http.MethodPost, base+"/approve", nil, testTenant, "principal-maker")
	if wMakerApprove.Code != http.StatusForbidden {
		t.Fatalf("SoD VIOLATION: expected 403 when maker attempts to approve own notice version, got %d", wMakerApprove.Code)
	}

	// 2. Checker approves version -> SUCCEEDS (200)
	wCheckerApprove := doRequestWithPrincipal(r, http.MethodPost, base+"/approve", nil, testTenant, "principal-checker")
	if wCheckerApprove.Code != http.StatusOK {
		t.Fatalf("expected 200 when checker approves notice version, got %d: %s", wCheckerApprove.Code, wCheckerApprove.Body.String())
	}

	// 3. Maker attempts to publish own version -> MUST be rejected (403)
	wMakerPublish := doRequestWithPrincipal(r, http.MethodPost, base+"/publish", nil, testTenant, "principal-maker")
	if wMakerPublish.Code != http.StatusForbidden {
		t.Fatalf("SoD VIOLATION: expected 403 when maker attempts to publish own notice version, got %d", wMakerPublish.Code)
	}

	// 4. Approver attempts to publish version -> MUST be rejected (403)
	wApproverPublish := doRequestWithPrincipal(r, http.MethodPost, base+"/publish", nil, testTenant, "principal-checker")
	if wApproverPublish.Code != http.StatusForbidden {
		t.Fatalf("SoD VIOLATION: expected 403 when approver attempts to publish notice version, got %d", wApproverPublish.Code)
	}

	// 5. Independent Publisher publishes -> SUCCEEDS (200)
	wPub := doRequestWithPrincipal(r, http.MethodPost, base+"/publish", nil, testTenant, "principal-publisher")
	if wPub.Code != http.StatusOK {
		t.Fatalf("expected 200 when publisher publishes notice version, got %d: %s", wPub.Code, wPub.Body.String())
	}
}

// ── Proxy / Authorized Representative Validation (§11.1) ──────────────────────

func TestConsent_ProxyValidation(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	// 1. IsProxy: true, but representative_subject_ref missing -> 400 Bad Request
	wMissingSubject := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef:                 "subject-ward",
		PurposeID:                  testPurpose,
		Action:                     "GRANTED",
		CaptureChannel:             "LEGAL_PORTAL",
		IsProxy:                    true,
		RepresentativeAuthorityRef: "POA-DOC-999",
	}, testTenant)
	if wMissingSubject.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when representative_subject_ref missing for proxy consent, got %d: %s", wMissingSubject.Code, wMissingSubject.Body.String())
	}

	// 2. IsProxy: true, but representative_authority_ref missing -> 400 Bad Request
	wMissingAuth := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef:               "subject-ward",
		PurposeID:                testPurpose,
		Action:                   "GRANTED",
		CaptureChannel:           "LEGAL_PORTAL",
		IsProxy:                  true,
		RepresentativeSubjectRef: "guardian-user",
	}, testTenant)
	if wMissingAuth.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when representative_authority_ref missing for proxy consent, got %d: %s", wMissingAuth.Code, wMissingAuth.Body.String())
	}

	// 3. IsProxy: true with all required fields -> 201 Created and preserves proxy fields
	repSub := "guardian-user"
	repAuth := "POA-DOC-999"
	repEv := "certified_power_of_attorney_signed_pdf"
	wValid := doRequest(r, http.MethodPost, "/privacy/consents", domain.RecordConsentRequest{
		SubjectRef:                 "subject-ward",
		PurposeID:                  testPurpose,
		Action:                     "GRANTED",
		CaptureChannel:             "LEGAL_PORTAL",
		IsProxy:                    true,
		RepresentativeSubjectRef:   repSub,
		RepresentativeAuthorityRef: repAuth,
		RepresentativeEvidence:     repEv,
		AffirmativeActionType:      "REPRESENTATIVE_SIGNATURE",
		AffirmativeEvidence:        "docusign_envelope_12345",
	}, testTenant)
	if wValid.Code != http.StatusCreated {
		t.Fatalf("expected 201 for valid proxy consent, got %d: %s", wValid.Code, wValid.Body.String())
	}
	var receipt domain.ConsentReceipt
	_ = json.Unmarshal(wValid.Body.Bytes(), &receipt)
	if !receipt.IsProxy {
		t.Fatalf("expected IsProxy=true, got false")
	}
	if receipt.RepresentativeSubjectRef == nil || *receipt.RepresentativeSubjectRef != repSub {
		t.Fatalf("expected representative_subject_ref=%s, got %v", repSub, receipt.RepresentativeSubjectRef)
	}
	if receipt.RepresentativeAuthorityRef == nil || *receipt.RepresentativeAuthorityRef != repAuth {
		t.Fatalf("expected representative_authority_ref=%s, got %v", repAuth, receipt.RepresentativeAuthorityRef)
	}
	if receipt.RepresentativeEvidence == nil || *receipt.RepresentativeEvidence != repEv {
		t.Fatalf("expected representative_evidence=%s, got %v", repEv, receipt.RepresentativeEvidence)
	}
	if receipt.AffirmativeActionType != "REPRESENTATIVE_SIGNATURE" {
		t.Fatalf("expected affirmative_action_type=REPRESENTATIVE_SIGNATURE, got %s", receipt.AffirmativeActionType)
	}
}

// ── PRV-N04: Replayed Consent Deduplication ──────────────────────────────────

func TestConsent_PRVN04_Deduplication(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	req := domain.RecordConsentRequest{
		SubjectRef:     "subject-dedup",
		PurposeID:      testPurpose,
		Action:         "GRANTED",
		CaptureChannel: "WEB_SIGNUP",
	}

	// First submission
	w1 := doRequest(r, http.MethodPost, "/privacy/consents", req, testTenant)
	if w1.Code != http.StatusCreated && w1.Code != http.StatusOK {
		t.Fatalf("expected 201 on first submission, got %d: %s", w1.Code, w1.Body.String())
	}
	var r1 domain.ConsentReceipt
	_ = json.Unmarshal(w1.Body.Bytes(), &r1)

	// Replay exact same submission
	w2 := doRequest(r, http.MethodPost, "/privacy/consents", req, testTenant)
	if w2.Code != http.StatusCreated && w2.Code != http.StatusOK {
		t.Fatalf("expected success on replayed consent, got %d: %s", w2.Code, w2.Body.String())
	}
	var r2 domain.ConsentReceipt
	_ = json.Unmarshal(w2.Body.Bytes(), &r2)

	// Must return existing receipt, NOT a new one
	if r1.ConsentReceiptID != r2.ConsentReceiptID {
		t.Fatalf("PRV-N04 VIOLATION: expected identical consent_receipt_id on replay, got %s vs %s", r1.ConsentReceiptID, r2.ConsentReceiptID)
	}
	if len(st.consentReceipts) != 1 {
		t.Fatalf("PRV-N04 VIOLATION: store must have exactly 1 receipt, got %d", len(st.consentReceipts))
	}
}

// ── Idempotency-Key (§18.1) Replay & Conflict ────────────────────────────────

func TestConsent_IdempotencyKey_ReplayAndConflict(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	key := "idem-key-test-999"
	headers := map[string]string{"Idempotency-Key": key}

	req1 := domain.RecordConsentRequest{
		SubjectRef:     "subject-idem-1",
		PurposeID:      testPurpose,
		Action:         "GRANTED",
		CaptureChannel: "MOBILE_APP",
	}

	// 1. Initial request with Idempotency-Key
	w1 := doRequestWithHeaders(r, http.MethodPost, "/privacy/consents", req1, testTenant, "principal-01", headers)
	if w1.Code != http.StatusCreated {
		t.Fatalf("expected 201 on initial request, got %d: %s", w1.Code, w1.Body.String())
	}

	// 2. Replay with identical payload and Idempotency-Key -> returns cached response with Idempotency-Replay header
	w2 := doRequestWithHeaders(r, http.MethodPost, "/privacy/consents", req1, testTenant, "principal-01", headers)
	if w2.Code != http.StatusCreated {
		t.Fatalf("expected 201 on idempotent replay, got %d: %s", w2.Code, w2.Body.String())
	}
	if w2.Header().Get("Idempotency-Replay") != "true" {
		t.Fatalf("expected Idempotency-Replay: true header, got %s", w2.Header().Get("Idempotency-Replay"))
	}

	// 3. Different payload with same Idempotency-Key -> 409 Conflict
	req2 := domain.RecordConsentRequest{
		SubjectRef:     "subject-DIFFERENT",
		PurposeID:      testPurpose,
		Action:         "DENIED",
		CaptureChannel: "WEB_SIGNUP",
	}
	w3 := doRequestWithHeaders(r, http.MethodPost, "/privacy/consents", req2, testTenant, "principal-01", headers)
	if w3.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on payload mismatch for same idempotency key, got %d: %s", w3.Code, w3.Body.String())
	}
}

// ── Canonical Notice Routes ──────────────────────────────────────────────────

func TestNotice_CanonicalRoutes(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	wCreate := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices", domain.CreateNoticeRequest{
		Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:canonical",
	}, testTenant, "principal-maker")
	var v domain.NoticeVersion
	_ = json.Unmarshal(wCreate.Body.Bytes(), &v)

	// Canonical route: POST /privacy/notices/{noticeID}/approve
	wApprove := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices/"+v.NoticeID+"/approve", nil, testTenant, "principal-approver")
	if wApprove.Code != http.StatusOK {
		t.Fatalf("expected 200 on canonical approve, got %d: %s", wApprove.Code, wApprove.Body.String())
	}

	// Canonical route: POST /privacy/notices/{noticeID}/publish
	wPublish := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices/"+v.NoticeID+"/publish", nil, testTenant, "principal-publisher")
	if wPublish.Code != http.StatusOK {
		t.Fatalf("expected 200 on canonical publish, got %d: %s", wPublish.Code, wPublish.Body.String())
	}

	// Canonical route: POST /privacy/notices/{noticeID}/presentation-receipts
	wPres := doRequest(r, http.MethodPost, "/privacy/notices/"+v.NoticeID+"/presentation-receipts", domain.RecordPresentationRequest{
		SubjectRef: "subject-canon",
		Channel:    "WEB",
		Locale:     "en-US",
	}, testTenant)
	if wPres.Code != http.StatusCreated {
		t.Fatalf("expected 201 on canonical presentation-receipts, got %d: %s", wPres.Code, wPres.Body.String())
	}

	// Canonical route: POST /privacy/notices/{noticeID}/withdraw
	wWithdraw := doRequestWithPrincipal(r, http.MethodPost, "/privacy/notices/"+v.NoticeID+"/withdraw", nil, testTenant, "principal-approver")
	if wWithdraw.Code != http.StatusOK {
		t.Fatalf("expected 200 on canonical withdraw, got %d: %s", wWithdraw.Code, wWithdraw.Body.String())
	}
}

