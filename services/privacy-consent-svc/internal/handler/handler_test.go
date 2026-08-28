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

func doRequest(r http.Handler, method, path string, body interface{}, tenantID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-01")
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

const testTenant = "tenant-consent-1"
const testPurpose = "purpose-marketing-emails"

func grantingPurposeRegistry() *stubPurposeRegistry {
	return &stubPurposeRegistry{published: map[string]bool{testPurpose: true}}
}

// ── notice lifecycle ─────────────────────────────────────────────────────────

func createPublishedNotice(t *testing.T, r http.Handler) *domain.NoticeVersion {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/notices", domain.CreateNoticeRequest{
		Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:abc123",
	}, testTenant)
	var v domain.NoticeVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	base := "/privacy/notices/" + v.NoticeID + "/versions/" + v.NoticeVersionID
	doRequest(r, http.MethodPost, base+"/approve", nil, testTenant)
	wPub := doRequest(r, http.MethodPost, base+"/publish", nil, testTenant)
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
	w := doRequest(r, http.MethodPost, base+"/withdraw", nil, testTenant)
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

	wVersion := doRequest(r, http.MethodPost, "/privacy/notices/"+first.NoticeID+"/versions", domain.CreateNoticeVersionRequest{
		ParentVersionID: first.NoticeVersionID, Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:def456",
	}, testTenant)
	var second domain.NoticeVersion
	_ = json.Unmarshal(wVersion.Body.Bytes(), &second)

	base := "/privacy/notices/" + second.NoticeID + "/versions/" + second.NoticeVersionID
	doRequest(r, http.MethodPost, base+"/approve", nil, testTenant)
	doRequest(r, http.MethodPost, base+"/publish", nil, testTenant)

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

	w := doRequest(r, http.MethodPost, "/privacy/notices", domain.CreateNoticeRequest{
		Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:abc123",
	}, testTenant)
	var v domain.NoticeVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	wPub := doRequest(r, http.MethodPost, "/privacy/notices/"+v.NoticeID+"/versions/"+v.NoticeVersionID+"/publish", nil, testTenant)
	if wPub.Code != http.StatusConflict {
		t.Fatalf("FABRICATION: expected 409 publishing a DRAFT (never approved) notice, got %d: %s", wPub.Code, wPub.Body.String())
	}
}

// ── presentation receipts ────────────────────────────────────────────────────

func TestRecordPresentation(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, grantingPurposeRegistry())

	notice := createPublishedNotice(t, r)
	w := doRequest(r, http.MethodPost, "/privacy/notices/"+notice.NoticeID+"/versions/"+notice.NoticeVersionID+"/presentation-receipts",
		domain.RecordPresentationRequest{SubjectRef: "subject-1", Channel: "WEB_SIGNUP", Locale: "en-US"}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
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

	wStatus := doRequest(r, http.MethodGet, "/privacy/consents?subject_ref=subject-1&purpose_id="+testPurpose, nil, testTenant)
	var res domain.ConsentResolution
	_ = json.Unmarshal(wStatus.Body.Bytes(), &res)
	if res.Status != domain.ConsentStatusWithdrawn {
		t.Fatalf("expected WITHDRAWN, got %s", res.Status)
	}
	if res.LatestReceipt == nil || res.LatestReceipt.Action != domain.ConsentActionGranted {
		t.Fatalf("PRV-I10 VIOLATION: original GRANTED receipt must remain visible and untouched, got %+v", res.LatestReceipt)
	}

	// Second withdrawal of the same receipt must be rejected, not silently re-recorded.
	wSecond := doRequest(r, http.MethodPost, "/privacy/consents/"+receipt.ConsentReceiptID+"/withdraw",
		domain.WithdrawConsentRequest{Channel: "ACCOUNT_SETTINGS"}, testTenant)
	if wSecond.Code != http.StatusConflict {
		t.Fatalf("expected 409 on double withdrawal, got %d: %s", wSecond.Code, wSecond.Body.String())
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
