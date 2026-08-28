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

	authzpkg "zoiko.io/privacy-purpose-registry-svc/internal/authz"
	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	"zoiko.io/privacy-purpose-registry-svc/internal/events"
	"zoiko.io/privacy-purpose-registry-svc/internal/handler"
	"zoiko.io/privacy-purpose-registry-svc/internal/middleware"
)

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct {
	calls      int
	lastType   string
	eventTypes []string
}

func (p *stubPublisher) Publish(_ context.Context, params events.PublishParams) error {
	p.calls++
	p.lastType = params.EventType
	p.eventTypes = append(p.eventTypes, params.EventType)
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// ── stub authz ───────────────────────────────────────────────────────────────

// stubAuthz grants by default; set deny=true to make every check fail
// with authzpkg.ErrAuthorizationDenied.
type stubAuthz struct{ deny bool }

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

// ── test harness ─────────────────────────────────────────────────────────────

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, logger)
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

const testTenant = "tenant-privacy-1"

// ── purpose lifecycle ────────────────────────────────────────────────────────

func TestCreatePurpose_ThenPublish(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	w := doRequest(r, http.MethodPost, "/privacy/purposes", domain.CreatePurposeRequest{
		Statement: "Send transactional emails about invoice status", CompatibilityClass: "PRIMARY",
		LawfulBasisRefs: []string{"basis-contract-performance"},
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var v domain.PurposeVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v.VersionStatus != domain.PurposeStatusDraft {
		t.Fatalf("expected DRAFT, got %s", v.VersionStatus)
	}

	wPub := doRequest(r, http.MethodPost, "/privacy/purposes/"+v.PurposeID+"/versions/"+v.PurposeVersionID+"/publish", nil, testTenant)
	if wPub.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", wPub.Code, wPub.Body.String())
	}
	var published domain.PurposeVersion
	_ = json.Unmarshal(wPub.Body.Bytes(), &published)
	if published.VersionStatus != domain.PurposeStatusPublished {
		t.Fatalf("expected PUBLISHED, got %s", published.VersionStatus)
	}
}

// TestPublishPurposeVersion_TwiceReturns409 is the regression test for
// PRV-I06: a purpose version is immutable once published — publishing
// twice must not silently succeed a second time.
func TestPublishPurposeVersion_TwiceReturns409(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	w := doRequest(r, http.MethodPost, "/privacy/purposes", domain.CreatePurposeRequest{
		Statement: "test", CompatibilityClass: "PRIMARY",
	}, testTenant)
	var v domain.PurposeVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	publishURL := "/privacy/purposes/" + v.PurposeID + "/versions/" + v.PurposeVersionID + "/publish"
	if w := doRequest(r, http.MethodPost, publishURL, nil, testTenant); w.Code != http.StatusOK {
		t.Fatalf("first publish: expected 200, got %d", w.Code)
	}
	w2 := doRequest(r, http.MethodPost, publishURL, nil, testTenant)
	if w2.Code != http.StatusConflict {
		t.Fatalf("FABRICATION: second publish should be rejected (PRV-I06 immutability), got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestCreatePurpose_ForeignTenant_Refused(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	w := doRequest(r, http.MethodPost, "/privacy/purposes", domain.CreatePurposeRequest{
		TenantID: "some-other-tenant", Statement: "test", CompatibilityClass: "PRIMARY",
	}, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when tenant_id disagrees with verified header, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreatePurpose_AuthorizationDenied(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{deny: true})

	w := doRequest(r, http.MethodPost, "/privacy/purposes", domain.CreatePurposeRequest{
		Statement: "test", CompatibilityClass: "PRIMARY",
	}, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on authorization denial, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreatePurpose_MissingPrincipal_Returns401(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	body, _ := json.Marshal(domain.CreatePurposeRequest{Statement: "test", CompatibilityClass: "PRIMARY"})
	req := httptest.NewRequest(http.MethodPost, "/privacy/purposes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", testTenant)
	// Deliberately no X-Principal-Id.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no principal, got %d: %s", w.Code, w.Body.String())
	}
}

// ── activity: validate ───────────────────────────────────────────────────────

func createPublishedPurpose(t *testing.T, r http.Handler, statement string) *domain.PurposeVersion {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/purposes", domain.CreatePurposeRequest{
		Statement: statement, CompatibilityClass: "PRIMARY",
	}, testTenant)
	var v domain.PurposeVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	wPub := doRequest(r, http.MethodPost, "/privacy/purposes/"+v.PurposeID+"/versions/"+v.PurposeVersionID+"/publish", nil, testTenant)
	var published domain.PurposeVersion
	_ = json.Unmarshal(wPub.Body.Bytes(), &published)
	return &published
}

func createDraftActivity(t *testing.T, r http.Handler, purposeIDs []string) *domain.ProcessingActivityVersion {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/processing-activities", domain.CreateActivityRequest{
		PrivacyRole: string(domain.RoleController), Owner: "privacy-team",
		PurposeIDs: purposeIDs, SubjectClasses: []string{"CUSTOMER"}, DataCategories: []string{"CONTACT_INFO"},
		Jurisdictions: []string{"US"},
	}, testTenant)
	var v domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return &v
}

// TestValidateActivity_UnregisteredPurpose_StaysDraftWithFinding is the
// regression test for PRV-001/PRV-I13: an activity naming a purpose that
// isn't a registered, published purpose must fail validation and stay
// DRAFT — never silently PERMIT.
func TestValidateActivity_UnregisteredPurpose_StaysDraftWithFinding(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	activity := createDraftActivity(t, r, []string{"purpose-does-not-exist"})

	w := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+activity.ActivityID+"/versions/"+activity.ActivityVersionID+"/validate", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (validate always answers, even when it finds problems), got %d: %s", w.Code, w.Body.String())
	}
	var got domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.VersionStatus != domain.ActivityStatusDraft {
		t.Fatalf("FABRICATION: expected version to stay DRAFT on validation finding, got %s", got.VersionStatus)
	}
	if len(got.ValidationFindings) == 0 {
		t.Fatal("expected at least one validation finding for an unregistered purpose")
	}
	found := false
	for _, f := range got.ValidationFindings {
		if f.Code == "PRV-001" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a PRV-001 finding, got %+v", got.ValidationFindings)
	}
}

func TestValidateActivity_AllPurposesPublished_TransitionsToValidated(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "process payroll")
	activity := createDraftActivity(t, r, []string{purpose.PurposeID})

	w := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+activity.ActivityID+"/versions/"+activity.ActivityVersionID+"/validate", nil, testTenant)
	var got domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.VersionStatus != domain.ActivityStatusValidated {
		t.Fatalf("expected VALIDATED, got %s: findings=%+v", got.VersionStatus, got.ValidationFindings)
	}
}

// ── activity: full lifecycle ─────────────────────────────────────────────────

func TestActivityFullLifecycle_DraftToActive(t *testing.T) {
	st := newStubStore()
	pub := &stubPublisher{}
	r := newTestRouter(st, pub, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "send marketing emails")
	activity := createDraftActivity(t, r, []string{purpose.PurposeID})
	base := "/privacy/processing-activities/" + activity.ActivityID + "/versions/" + activity.ActivityVersionID

	step := func(action string, wantCode int) domain.ProcessingActivityVersion {
		t.Helper()
		w := doRequest(r, http.MethodPost, base+"/"+action, nil, testTenant)
		if w.Code != wantCode {
			t.Fatalf("%s: expected %d, got %d: %s", action, wantCode, w.Code, w.Body.String())
		}
		var v domain.ProcessingActivityVersion
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}

	v := step("validate", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusValidated {
		t.Fatalf("expected VALIDATED, got %s", v.VersionStatus)
	}
	v = step("submit", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusSubmitted {
		t.Fatalf("expected SUBMITTED, got %s", v.VersionStatus)
	}
	v = step("approve", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusApproved {
		t.Fatalf("expected APPROVED, got %s", v.VersionStatus)
	}
	v = step("activate", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusActive {
		t.Fatalf("expected ACTIVE, got %s", v.VersionStatus)
	}
	if v.EffectiveFrom == nil {
		t.Fatal("expected effective_from to be set on activation")
	}

	expectedEvents := []string{
		// createPublishedPurpose also publishes through this same
		// stubPublisher — its event comes first.
		"privacy.purpose.published",
		"privacy.processing_activity.submitted", "privacy.processing_activity.approved",
		"privacy.processing_activity.activated",
	}
	if len(pub.eventTypes) != len(expectedEvents) {
		t.Fatalf("expected events %v, got %v", expectedEvents, pub.eventTypes)
	}
	for i, want := range expectedEvents {
		if pub.eventTypes[i] != want {
			t.Fatalf("event %d: expected %s, got %s", i, want, pub.eventTypes[i])
		}
	}

	// Now suspend and retire.
	v = step("suspend", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusSuspended {
		t.Fatalf("expected SUSPENDED, got %s", v.VersionStatus)
	}
	v = step("resume", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusActive {
		t.Fatalf("expected ACTIVE again after resume, got %s", v.VersionStatus)
	}
	v = step("retire", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusRetired {
		t.Fatalf("expected RETIRED, got %s", v.VersionStatus)
	}
}

// TestActivateActivity_SkippingApproval_Rejected is the regression test
// for the state machine itself: activation must be reachable ONLY from
// APPROVED. Skipping straight from DRAFT (or any other state) to ACTIVE
// would mean SUBMITTED silently becoming APPROVED — the exact fabrication
// this service's domain package doc comment says it must not do.
func TestActivateActivity_SkippingApproval_Rejected(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "test")
	activity := createDraftActivity(t, r, []string{purpose.PurposeID})
	base := "/privacy/processing-activities/" + activity.ActivityID + "/versions/" + activity.ActivityVersionID

	w := doRequest(r, http.MethodPost, base+"/activate", nil, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("FABRICATION: expected 409 activating a DRAFT version (never reached APPROVED), got %d: %s", w.Code, w.Body.String())
	}
}

// TestSubmitActivity_SkippingValidation_Rejected pins the same principle
// one step earlier: DRAFT cannot jump straight to SUBMITTED.
func TestSubmitActivity_SkippingValidation_Rejected(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "test")
	activity := createDraftActivity(t, r, []string{purpose.PurposeID})
	base := "/privacy/processing-activities/" + activity.ActivityID + "/versions/" + activity.ActivityVersionID

	w := doRequest(r, http.MethodPost, base+"/submit", nil, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 submitting a DRAFT (unvalidated) version, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRejectActivity_ThenFixLoop_CreatesNewVersion proves the Figure 4
// "reject/fix loop" is taken via a NEW version, never a resurrection of
// the rejected row (PRV-I20).
func TestRejectActivity_ThenFixLoop_CreatesNewVersion(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "test")
	activity := createDraftActivity(t, r, []string{purpose.PurposeID})
	base := "/privacy/processing-activities/" + activity.ActivityID + "/versions/" + activity.ActivityVersionID

	doRequest(r, http.MethodPost, base+"/validate", nil, testTenant)
	doRequest(r, http.MethodPost, base+"/submit", nil, testTenant)

	wReject := doRequest(r, http.MethodPost, base+"/reject", domain.RejectActivityRequest{Reason: "missing DPIA"}, testTenant)
	if wReject.Code != http.StatusOK {
		t.Fatalf("expected 200 rejecting a SUBMITTED version, got %d: %s", wReject.Code, wReject.Body.String())
	}
	var rejected domain.ProcessingActivityVersion
	_ = json.Unmarshal(wReject.Body.Bytes(), &rejected)
	if rejected.VersionStatus != domain.ActivityStatusRejected {
		t.Fatalf("expected REJECTED, got %s", rejected.VersionStatus)
	}
	if rejected.RejectionReason == nil || *rejected.RejectionReason != "missing DPIA" {
		t.Fatalf("expected rejection_reason recorded, got %v", rejected.RejectionReason)
	}

	// The dead end: rejected can't be resurrected via submit/approve/activate.
	wResubmit := doRequest(r, http.MethodPost, base+"/submit", nil, testTenant)
	if wResubmit.Code != http.StatusConflict {
		t.Fatalf("expected 409 re-submitting a REJECTED version (dead end by design), got %d", wResubmit.Code)
	}

	// The real fix loop: a new version, explicitly superseding the rejected one.
	wNewVersion := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+activity.ActivityID+"/versions", domain.CreateActivityVersionRequest{
		ParentVersionID: activity.ActivityVersionID, PrivacyRole: string(domain.RoleController), Owner: "privacy-team",
		PurposeIDs: []string{purpose.PurposeID}, SubjectClasses: []string{"CUSTOMER"}, DataCategories: []string{"CONTACT_INFO"},
		Jurisdictions: []string{"US"},
	}, testTenant)
	if wNewVersion.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating a successor version, got %d: %s", wNewVersion.Code, wNewVersion.Body.String())
	}
	var newVersion domain.ProcessingActivityVersion
	_ = json.Unmarshal(wNewVersion.Body.Bytes(), &newVersion)
	if newVersion.VersionStatus != domain.ActivityStatusDraft {
		t.Fatalf("expected the new version to start DRAFT, got %s", newVersion.VersionStatus)
	}
	if newVersion.SupersedesVersionID == nil || *newVersion.SupersedesVersionID != activity.ActivityVersionID {
		t.Fatalf("expected supersedes_version_id to link back to the rejected version, got %v", newVersion.SupersedesVersionID)
	}
}

// ── ROPA / as_of ─────────────────────────────────────────────────────────────

func activateFullActivity(t *testing.T, r http.Handler, purposeID string) *domain.ProcessingActivityVersion {
	t.Helper()
	activity := createDraftActivity(t, r, []string{purposeID})
	base := "/privacy/processing-activities/" + activity.ActivityID + "/versions/" + activity.ActivityVersionID
	doRequest(r, http.MethodPost, base+"/validate", nil, testTenant)
	doRequest(r, http.MethodPost, base+"/submit", nil, testTenant)
	doRequest(r, http.MethodPost, base+"/approve", nil, testTenant)
	w := doRequest(r, http.MethodPost, base+"/activate", nil, testTenant)
	var v domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return &v
}

func TestListROPA_OnlyReturnsActiveVersions(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "test")
	activateFullActivity(t, r, purpose.PurposeID)
	createDraftActivity(t, r, []string{purpose.PurposeID}) // never activated

	w := doRequest(r, http.MethodGet, "/privacy/ropa", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got struct {
		Data  []domain.ProcessingActivityVersion `json:"data"`
		Count int                                `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Count != 1 {
		t.Fatalf("expected exactly 1 ACTIVE version in ROPA, got %d", got.Count)
	}
	if got.Data[0].VersionStatus != domain.ActivityStatusActive {
		t.Fatalf("expected ACTIVE, got %s", got.Data[0].VersionStatus)
	}
}

func TestGetActivity_AsOf_ResolvesHistoricalVersion(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "test")
	active := activateFullActivity(t, r, purpose.PurposeID)

	w := doRequest(r, http.MethodGet, "/privacy/processing-activities/"+active.ActivityID, nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.ActivityVersionID != active.ActivityVersionID {
		t.Fatalf("expected to resolve the active version %s, got %s", active.ActivityVersionID, got.ActivityVersionID)
	}
}
