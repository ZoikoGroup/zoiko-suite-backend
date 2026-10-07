package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/workflow-svc/internal/domain"
)

func auditEngagement() *domain.AuditEngagement {
	return &domain.AuditEngagement{EngagementID: "eng-1", LegalEntityID: "le-1", Status: domain.AuditEngagementProposed}
}

func validAuditEngagementBody() string {
	return `{"tenant_id":"t-1","legal_entity_id":"le-1","engagement_code":"FY26-STAT","engagement_type":"STATUTORY_AUDIT","reporting_period_start":"2025-04-01T00:00:00Z","reporting_period_end":"2026-03-31T00:00:00Z","framework_profile_id":"isa","framework_profile_version":"2025.1","methodology_id":"firm-method","methodology_version":"2026.1","responsible_partner_id":"partner-1","scope_summary":"Annual statutory audit"}`
}

func TestCreateAuditEngagement_Created(t *testing.T) {
	s := &stubStore{auditEngagement: auditEngagement(), auditCreateCreated: true}
	r := newTestRouterFull(s, &stubPublisher{}, &stubAuthz{})
	req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/audit/engagements/", bytes.NewBufferString(validAuditEngagementBody())), "manager-1")
	req.Header.Set("X-Correlation-ID", "audit-create-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateAuditEngagement_RequiresCorrelation(t *testing.T) {
	r := newTestRouter(&stubStore{})
	req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/audit/engagements/", bytes.NewBufferString(validAuditEngagementBody())), "manager-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateAuditEngagement_RejectsForeignTenant(t *testing.T) {
	r := newTestRouter(&stubStore{})
	body := strings.Replace(validAuditEngagementBody(), `"tenant_id":"t-1"`, `"tenant_id":"other"`, 1)
	req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/audit/engagements/", bytes.NewBufferString(body)), "manager-1")
	req.Header.Set("X-Correlation-ID", "audit-create-foreign")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestSubmitAuditEngagementAcceptance_RequiresEvidence(t *testing.T) {
	s := &stubStore{auditEngagement: auditEngagement()}
	r := newTestRouterFull(s, &stubPublisher{}, &stubAuthz{})
	req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/audit/engagements/eng-1/submit-acceptance", bytes.NewBufferString(`{}`)), "manager-1")
	req.Header.Set("X-Correlation-ID", "audit-accept-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestSubmitAuditEngagementAcceptance_PinsVerifiedDocumentVersion(t *testing.T) {
	e := auditEngagement()
	e.Status = domain.AuditEngagementProposed
	s := &stubStore{auditEngagement: e, auditSubmitChanged: true}
	r := newTestRouterFull(s, &stubPublisher{}, &stubAuthz{})
	req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/audit/engagements/eng-1/submit-acceptance", bytes.NewBufferString(`{"document_id":"doc-1"}`)), "manager-1")
	req.Header.Set("X-Correlation-ID", "audit-accept-verified")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetAuditEngagement_AuthorizationDenied(t *testing.T) {
	s := &stubStore{auditEngagement: auditEngagement()}
	r := newTestRouterFull(s, &stubPublisher{}, &stubAuthz{err: domain.ErrAuthorizationDenied})
	req := scopedAs(httptest.NewRequest(http.MethodGet, "/v1/audit/engagements/eng-1", nil), "reader-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestRecordAuditAcceptanceDecision_RefusesCreator(t *testing.T) {
	e := auditEngagement()
	e.Status, e.CreatedByPrincipalID = domain.AuditEngagementAcceptanceReview, "manager-1"
	r := newTestRouterFull(&stubStore{auditEngagement: e}, &stubPublisher{}, &stubAuthz{})
	req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/audit/engagements/eng-1/acceptance-decision", bytes.NewBufferString(`{"decision":"ACCEPT"}`)), "manager-1")
	req.Header.Set("X-Correlation-ID", "audit-decision-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}
