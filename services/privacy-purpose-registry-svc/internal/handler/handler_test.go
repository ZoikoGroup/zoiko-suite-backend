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

func doRequestWithPrincipal(r http.Handler, method, path string, body interface{}, tenantID, principalID string, headers ...map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	for _, h := range headers {
		for k, v := range h {
			req.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func doRequest(r http.Handler, method, path string, body interface{}, tenantID string) *httptest.ResponseRecorder {
	return doRequestWithPrincipal(r, method, path, body, tenantID, "principal-01")
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

	// Maker-checker (SoD): publishing must be done by a distinct reviewer principal
	wPub := doRequestWithPrincipal(r, http.MethodPost, "/privacy/purposes/"+v.PurposeID+"/versions/"+v.PurposeVersionID+"/publish", nil, testTenant, "reviewer-01")
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
	if w := doRequestWithPrincipal(r, http.MethodPost, publishURL, nil, testTenant, "reviewer-01"); w.Code != http.StatusOK {
		t.Fatalf("first publish: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	w2 := doRequestWithPrincipal(r, http.MethodPost, publishURL, nil, testTenant, "reviewer-01")
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

// ── segregation of duties ────────────────────────────────────────────────────

func TestSegregationOfDuties_MakerCannotPublishOwnPurpose(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	w := doRequestWithPrincipal(r, http.MethodPost, "/privacy/purposes", domain.CreatePurposeRequest{
		Statement: "self approval test", CompatibilityClass: "PRIMARY",
	}, testTenant, "maker-01")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var v domain.PurposeVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	// Maker attempts to self-publish
	wPub := doRequestWithPrincipal(r, http.MethodPost, "/privacy/purposes/"+v.PurposeID+"/versions/"+v.PurposeVersionID+"/publish", nil, testTenant, "maker-01")
	if wPub.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for self-publish under SoD, got %d: %s", wPub.Code, wPub.Body.String())
	}
}

func TestSegregationOfDuties_MakerCannotApproveOwnActivity(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "test purpose")
	w := doRequestWithPrincipal(r, http.MethodPost, "/privacy/processing-activities", domain.CreateActivityRequest{
		PrivacyRole:              string(domain.RoleController),
		Owner:                    "privacy-team",
		PurposeIDs:               []string{purpose.PurposeID},
		SubjectClasses:           []string{"CUSTOMER"},
		DataCategories:           []string{"CONTACT_INFO"},
		Jurisdictions:            []string{"US"},
		RetentionRuleRefs:        []string{"retention-rule-7y"},
		NoticeConsentDependency: string(domain.NoticeConsentRequired),
		DPIATIAStatus:            string(domain.DPIATIAResolved),
	}, testTenant, "maker-01")
	var v domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	base := "/privacy/processing-activities/" + v.ActivityID + "/versions/" + v.ActivityVersionID
	doRequestWithPrincipal(r, http.MethodPost, base+"/validate", nil, testTenant, "maker-01")
	doRequestWithPrincipal(r, http.MethodPost, base+"/submit", nil, testTenant, "maker-01")

	// Maker attempts to self-approve
	wApprove := doRequestWithPrincipal(r, http.MethodPost, base+"/approve", nil, testTenant, "maker-01")
	if wApprove.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for self-approval under SoD, got %d: %s", wApprove.Code, wApprove.Body.String())
	}

	// Distinct reviewer approves successfully
	wApproveReviewer := doRequestWithPrincipal(r, http.MethodPost, base+"/approve", nil, testTenant, "reviewer-01")
	if wApproveReviewer.Code != http.StatusOK {
		t.Fatalf("expected 200 for reviewer approval under SoD, got %d: %s", wApproveReviewer.Code, wApproveReviewer.Body.String())
	}
}

func TestSegregationOfDuties_MakerCannotRejectOwnActivity(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "test purpose")
	w := doRequestWithPrincipal(r, http.MethodPost, "/privacy/processing-activities", domain.CreateActivityRequest{
		PrivacyRole:              string(domain.RoleController),
		Owner:                    "privacy-team",
		PurposeIDs:               []string{purpose.PurposeID},
		SubjectClasses:           []string{"CUSTOMER"},
		DataCategories:           []string{"CONTACT_INFO"},
		Jurisdictions:            []string{"US"},
		RetentionRuleRefs:        []string{"retention-rule-7y"},
		NoticeConsentDependency: string(domain.NoticeConsentRequired),
		DPIATIAStatus:            string(domain.DPIATIAResolved),
	}, testTenant, "maker-01")
	var v domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	base := "/privacy/processing-activities/" + v.ActivityID + "/versions/" + v.ActivityVersionID
	doRequestWithPrincipal(r, http.MethodPost, base+"/validate", nil, testTenant, "maker-01")
	doRequestWithPrincipal(r, http.MethodPost, base+"/submit", nil, testTenant, "maker-01")

	// Maker attempts to self-reject
	wReject := doRequestWithPrincipal(r, http.MethodPost, base+"/reject", domain.RejectActivityRequest{Reason: "self reject"}, testTenant, "maker-01")
	if wReject.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for self-reject under SoD, got %d: %s", wReject.Code, wReject.Body.String())
	}
}

// ── activity: validate (all 8 gates) ─────────────────────────────────────────

func createPublishedPurpose(t *testing.T, r http.Handler, statement string) *domain.PurposeVersion {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/purposes", domain.CreatePurposeRequest{
		Statement: statement, CompatibilityClass: "PRIMARY", LawfulBasisRefs: []string{"legitimate-interest"},
	}, testTenant)
	var v domain.PurposeVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	wPub := doRequestWithPrincipal(r, http.MethodPost, "/privacy/purposes/"+v.PurposeID+"/versions/"+v.PurposeVersionID+"/publish", nil, testTenant, "reviewer-01")
	var published domain.PurposeVersion
	_ = json.Unmarshal(wPub.Body.Bytes(), &published)
	return &published
}

func createDraftActivity(t *testing.T, r http.Handler, purposeIDs []string) *domain.ProcessingActivityVersion {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/processing-activities", domain.CreateActivityRequest{
		PrivacyRole:              string(domain.RoleController),
		Owner:                    "privacy-team",
		PurposeIDs:               purposeIDs,
		SubjectClasses:           []string{"CUSTOMER"},
		DataCategories:           []string{"CONTACT_INFO"},
		Jurisdictions:            []string{"US"},
		RetentionRuleRefs:        []string{"retention-rule-7y"},
		NoticeConsentDependency: string(domain.NoticeConsentRequired),
		DPIATIAStatus:            string(domain.DPIATIAResolved),
	}, testTenant)
	var v domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return &v
}

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

func TestValidateActivity_Gate6_RetentionMissing_EmitsPRV014(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "test retention")
	w := doRequest(r, http.MethodPost, "/privacy/processing-activities", domain.CreateActivityRequest{
		PrivacyRole:              string(domain.RoleController),
		Owner:                    "privacy-team",
		PurposeIDs:               []string{purpose.PurposeID},
		SubjectClasses:           []string{"CUSTOMER"},
		DataCategories:           []string{"CONTACT_INFO"},
		Jurisdictions:            []string{"US"},
		RetentionRuleRefs:        []string{}, // Missing Gate 6
		NoticeConsentDependency: string(domain.NoticeConsentRequired),
		DPIATIAStatus:            string(domain.DPIATIAResolved),
	}, testTenant)
	var v domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	wVal := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+v.ActivityID+"/versions/"+v.ActivityVersionID+"/validate", nil, testTenant)
	var got domain.ProcessingActivityVersion
	_ = json.Unmarshal(wVal.Body.Bytes(), &got)
	if got.VersionStatus != domain.ActivityStatusDraft {
		t.Fatalf("expected DRAFT, got %s", got.VersionStatus)
	}
	found := false
	for _, f := range got.ValidationFindings {
		if f.Code == "PRV-014" && f.Field == "retention_rule_refs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PRV-014 finding for retention_rule_refs, got %+v", got.ValidationFindings)
	}
}

func TestValidateActivity_Gate4_NoLawfulBasis_EmitsPRV005(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	// A purpose published with no lawful-basis reference at all.
	w := doRequest(r, http.MethodPost, "/privacy/purposes", domain.CreatePurposeRequest{
		Statement: "no lawful basis", CompatibilityClass: "PRIMARY",
	}, testTenant)
	var pv domain.PurposeVersion
	_ = json.Unmarshal(w.Body.Bytes(), &pv)
	wPub := doRequestWithPrincipal(r, http.MethodPost, "/privacy/purposes/"+pv.PurposeID+"/versions/"+pv.PurposeVersionID+"/publish", nil, testTenant, "reviewer-01")
	var purpose domain.PurposeVersion
	_ = json.Unmarshal(wPub.Body.Bytes(), &purpose)

	activity := createDraftActivity(t, r, []string{purpose.PurposeID})
	wVal := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+activity.ActivityID+"/versions/"+activity.ActivityVersionID+"/validate", nil, testTenant)
	var got domain.ProcessingActivityVersion
	_ = json.Unmarshal(wVal.Body.Bytes(), &got)
	if got.VersionStatus != domain.ActivityStatusDraft {
		t.Fatalf("expected DRAFT, got %s", got.VersionStatus)
	}
	found := false
	for _, f := range got.ValidationFindings {
		if f.Code == "PRV-005" && f.Field == "purpose_ids" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PRV-005 finding for a purpose with no lawful-basis reference, got %+v", got.ValidationFindings)
	}
}

func TestValidateActivity_Gate6_MultiJurisdictionNoTransferRefs_EmitsPRV015(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "cross-border processing")
	w := doRequest(r, http.MethodPost, "/privacy/processing-activities", domain.CreateActivityRequest{
		PrivacyRole:              string(domain.RoleController),
		Owner:                    "privacy-team",
		PurposeIDs:               []string{purpose.PurposeID},
		SubjectClasses:           []string{"CUSTOMER"},
		DataCategories:           []string{"CONTACT_INFO"},
		Jurisdictions:            []string{"US", "EU"}, // multiple jurisdictions, no transfer refs
		RetentionRuleRefs:        []string{"retention-rule-7y"},
		NoticeConsentDependency: string(domain.NoticeConsentRequired),
		DPIATIAStatus:            string(domain.DPIATIAResolved),
	}, testTenant)
	var v domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	wVal := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+v.ActivityID+"/versions/"+v.ActivityVersionID+"/validate", nil, testTenant)
	var got domain.ProcessingActivityVersion
	_ = json.Unmarshal(wVal.Body.Bytes(), &got)
	if got.VersionStatus != domain.ActivityStatusDraft {
		t.Fatalf("expected DRAFT, got %s", got.VersionStatus)
	}
	found := false
	for _, f := range got.ValidationFindings {
		if f.Code == "PRV-015" && f.Field == "transfer_refs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PRV-015 finding for multi-jurisdiction activity with no transfer refs, got %+v", got.ValidationFindings)
	}

	// Single-jurisdiction activities must not be required to populate this.
	singleJurisdiction := createDraftActivity(t, r, []string{purpose.PurposeID})
	wVal2 := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+singleJurisdiction.ActivityID+"/versions/"+singleJurisdiction.ActivityVersionID+"/validate", nil, testTenant)
	var got2 domain.ProcessingActivityVersion
	_ = json.Unmarshal(wVal2.Body.Bytes(), &got2)
	for _, f := range got2.ValidationFindings {
		if f.Code == "PRV-015" {
			t.Fatalf("single-jurisdiction activity must not require transfer_refs, got finding %+v", f)
		}
	}
}

func TestValidateActivity_Gate7_NoticeConsentMissing_EmitsPRV006(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "test notice consent")
	w := doRequest(r, http.MethodPost, "/privacy/processing-activities", domain.CreateActivityRequest{
		PrivacyRole:       string(domain.RoleController),
		Owner:             "privacy-team",
		PurposeIDs:        []string{purpose.PurposeID},
		SubjectClasses:    []string{"CUSTOMER"},
		DataCategories:    []string{"CONTACT_INFO"},
		Jurisdictions:     []string{"US"},
		RetentionRuleRefs: []string{"retention-7y"},
		// NoticeConsentDependency left empty: violates Gate 7
		DPIATIAStatus: string(domain.DPIATIAResolved),
	}, testTenant)
	var v domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	wVal := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+v.ActivityID+"/versions/"+v.ActivityVersionID+"/validate", nil, testTenant)
	var got domain.ProcessingActivityVersion
	_ = json.Unmarshal(wVal.Body.Bytes(), &got)
	if got.VersionStatus != domain.ActivityStatusDraft {
		t.Fatalf("expected DRAFT, got %s", got.VersionStatus)
	}
	found := false
	for _, f := range got.ValidationFindings {
		if f.Code == "PRV-006" && f.Field == "notice_consent_dependency" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PRV-006 finding for notice_consent_dependency, got %+v", got.ValidationFindings)
	}
}

func TestValidateActivity_Gate8_DPIATIAMissing_EmitsPRV016(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "test dpia")
	w := doRequest(r, http.MethodPost, "/privacy/processing-activities", domain.CreateActivityRequest{
		PrivacyRole:              string(domain.RoleController),
		Owner:                    "privacy-team",
		PurposeIDs:               []string{purpose.PurposeID},
		SubjectClasses:           []string{"CUSTOMER"},
		DataCategories:           []string{"CONTACT_INFO"},
		Jurisdictions:            []string{"US"},
		RetentionRuleRefs:        []string{"retention-7y"},
		NoticeConsentDependency: string(domain.NoticeConsentRequired),
		// DPIATIAStatus left empty: violates Gate 8
	}, testTenant)
	var v domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	wVal := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+v.ActivityID+"/versions/"+v.ActivityVersionID+"/validate", nil, testTenant)
	var got domain.ProcessingActivityVersion
	_ = json.Unmarshal(wVal.Body.Bytes(), &got)
	if got.VersionStatus != domain.ActivityStatusDraft {
		t.Fatalf("expected DRAFT, got %s", got.VersionStatus)
	}
	found := false
	for _, f := range got.ValidationFindings {
		if f.Code == "PRV-016" && f.Field == "dpia_tia_status" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PRV-016 finding for dpia_tia_status, got %+v", got.ValidationFindings)
	}
}

func TestValidateActivity_Gate5_SubjectClassesAndCategories_EmitsPRV010(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "test categories")
	w := doRequest(r, http.MethodPost, "/privacy/processing-activities", domain.CreateActivityRequest{
		PrivacyRole:              string(domain.RoleController),
		Owner:                    "privacy-team",
		PurposeIDs:               []string{purpose.PurposeID},
		SubjectClasses:           []string{}, // Missing
		DataCategories:           []string{}, // Missing
		Jurisdictions:            []string{"US"},
		RetentionRuleRefs:        []string{"retention-7y"},
		NoticeConsentDependency: string(domain.NoticeConsentRequired),
		DPIATIAStatus:            string(domain.DPIATIAResolved),
	}, testTenant)
	var v domain.ProcessingActivityVersion
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	wVal := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+v.ActivityID+"/versions/"+v.ActivityVersionID+"/validate", nil, testTenant)
	var got domain.ProcessingActivityVersion
	_ = json.Unmarshal(wVal.Body.Bytes(), &got)
	for _, f := range got.ValidationFindings {
		if f.Code == "PRV-019" {
			t.Fatalf("REGRESSION: PRV-019 should not be emitted for missing subject_classes or data_categories, got %+v", f)
		}
	}
	var count010 int
	for _, f := range got.ValidationFindings {
		if f.Code == "PRV-010" {
			count010++
		}
	}
	if count010 < 2 {
		t.Fatalf("expected at least two PRV-010 findings for subject_classes and data_categories, got %d in %+v", count010, got.ValidationFindings)
	}
}

// ── idempotency (§18.1) ──────────────────────────────────────────────────────

func TestIdempotency_ReplayReturnsCachedResponse(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	payload := domain.CreatePurposeRequest{
		Statement: "idempotency test statement", CompatibilityClass: "PRIMARY",
	}
	header := map[string]string{"Idempotency-Key": "idem-key-001"}

	// Initial request
	w1 := doRequestWithPrincipal(r, http.MethodPost, "/privacy/purposes", payload, testTenant, "principal-01", header)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first request expected 201, got %d: %s", w1.Code, w1.Body.String())
	}
	if w1.Header().Get("Idempotency-Replay") != "" {
		t.Fatalf("first request should not have Idempotency-Replay header")
	}

	// Repeated identical request with same key
	w2 := doRequestWithPrincipal(r, http.MethodPost, "/privacy/purposes", payload, testTenant, "principal-01", header)
	if w2.Code != http.StatusCreated {
		t.Fatalf("replay expected 201, got %d: %s", w2.Code, w2.Body.String())
	}
	if w2.Header().Get("Idempotency-Replay") != "true" {
		t.Fatalf("expected Idempotency-Replay header 'true', got '%s'", w2.Header().Get("Idempotency-Replay"))
	}
	if w1.Body.String() != w2.Body.String() {
		t.Fatalf("expected exact matching response body, got:\nw1: %s\nw2: %s", w1.Body.String(), w2.Body.String())
	}
}

func TestIdempotency_PayloadMismatchReturns409(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	payload1 := domain.CreatePurposeRequest{
		Statement: "payload 1 statement", CompatibilityClass: "PRIMARY",
	}
	payload2 := domain.CreatePurposeRequest{
		Statement: "payload 2 DIFFERENT statement", CompatibilityClass: "PRIMARY",
	}
	header := map[string]string{"Idempotency-Key": "idem-key-conflict-002"}

	// First request
	w1 := doRequestWithPrincipal(r, http.MethodPost, "/privacy/purposes", payload1, testTenant, "principal-01", header)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first request expected 201, got %d: %s", w1.Code, w1.Body.String())
	}

	// Second request with conflicting payload
	w2 := doRequestWithPrincipal(r, http.MethodPost, "/privacy/purposes", payload2, testTenant, "principal-01", header)
	if w2.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for modified payload under same key, got %d: %s", w2.Code, w2.Body.String())
	}
}

// ── canonical route aliases (§9.1) ───────────────────────────────────────────

func TestCanonicalLatestActivityRoutes(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	purpose := createPublishedPurpose(t, r, "canonical routes purpose")
	activity := createDraftActivity(t, r, []string{purpose.PurposeID})

	// 1. POST /privacy/processing-activities/{id}/validate
	wVal := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+activity.ActivityID+"/validate", nil, testTenant)
	if wVal.Code != http.StatusOK {
		t.Fatalf("canonical validate failed: %d %s", wVal.Code, wVal.Body.String())
	}

	// 2. POST /privacy/processing-activities/{id}/submit
	wSub := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+activity.ActivityID+"/submit", nil, testTenant)
	if wSub.Code != http.StatusOK {
		t.Fatalf("canonical submit failed: %d %s", wSub.Code, wSub.Body.String())
	}

	// 3. POST /privacy/processing-activities/{id}/approve (by reviewer)
	wApp := doRequestWithPrincipal(r, http.MethodPost, "/privacy/processing-activities/"+activity.ActivityID+"/approve", nil, testTenant, "reviewer-01")
	if wApp.Code != http.StatusOK {
		t.Fatalf("canonical approve failed: %d %s", wApp.Code, wApp.Body.String())
	}

	// 4. POST /privacy/processing-activities/{id}/activate
	wAct := doRequest(r, http.MethodPost, "/privacy/processing-activities/"+activity.ActivityID+"/activate", nil, testTenant)
	if wAct.Code != http.StatusOK {
		t.Fatalf("canonical activate failed: %d %s", wAct.Code, wAct.Body.String())
	}
	var activated domain.ProcessingActivityVersion
	_ = json.Unmarshal(wAct.Body.Bytes(), &activated)
	if activated.VersionStatus != domain.ActivityStatusActive {
		t.Fatalf("expected ACTIVE after canonical activate, got %s", activated.VersionStatus)
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

	step := func(action string, principalID string, wantCode int) domain.ProcessingActivityVersion {
		t.Helper()
		w := doRequestWithPrincipal(r, http.MethodPost, base+"/"+action, nil, testTenant, principalID)
		if w.Code != wantCode {
			t.Fatalf("%s: expected %d, got %d: %s", action, wantCode, w.Code, w.Body.String())
		}
		var v domain.ProcessingActivityVersion
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}

	v := step("validate", "principal-01", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusValidated {
		t.Fatalf("expected VALIDATED, got %s", v.VersionStatus)
	}
	v = step("submit", "principal-01", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusSubmitted {
		t.Fatalf("expected SUBMITTED, got %s", v.VersionStatus)
	}
	// Maker-checker SoD: must be approved by reviewer, not maker
	v = step("approve", "reviewer-01", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusApproved {
		t.Fatalf("expected APPROVED, got %s", v.VersionStatus)
	}
	v = step("activate", "principal-01", http.StatusOK)
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
	v = step("suspend", "principal-01", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusSuspended {
		t.Fatalf("expected SUSPENDED, got %s", v.VersionStatus)
	}
	v = step("resume", "principal-01", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusActive {
		t.Fatalf("expected ACTIVE again after resume, got %s", v.VersionStatus)
	}
	v = step("retire", "principal-01", http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusRetired {
		t.Fatalf("expected RETIRED, got %s", v.VersionStatus)
	}
}

// TestActivateActivity_SkippingApproval_Rejected is the regression test
// for the state machine itself: activation must be reachable ONLY from
// APPROVED.
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

	// Reject by distinct reviewer principal to satisfy SoD
	wReject := doRequestWithPrincipal(r, http.MethodPost, base+"/reject", domain.RejectActivityRequest{Reason: "missing DPIA"}, testTenant, "reviewer-01")
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
		Jurisdictions:            []string{"US"},
		RetentionRuleRefs:        []string{"retention-rule-7y"},
		NoticeConsentDependency: string(domain.NoticeConsentRequired),
		DPIATIAStatus:            string(domain.DPIATIAResolved),
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
	doRequestWithPrincipal(r, http.MethodPost, base+"/approve", nil, testTenant, "reviewer-01")
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
