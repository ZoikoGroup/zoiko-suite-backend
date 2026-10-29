package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	apicontract "zoiko.io/identity-envelope/apicontract"
	"zoiko.io/workflow-svc/internal/domain"
	svcenvelope "zoiko.io/workflow-svc/internal/envelope"
	"zoiko.io/workflow-svc/internal/handler"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
)

// API-2 (ZS-API-001 §12): every refusal is RFC 9457 problem+json with a
// stable code — including the map[string]string bodies most handlers write,
// which writeJSON once let through as legacy application/json. 412/428 are
// not answered here until API-3.
func TestAPI2_ErrorResponsesAreProblemDetails(t *testing.T) {
	serve := func(r http.Handler, req *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	pending := &domain.WorkflowInstance{WorkflowInstanceID: testWorkflowID, LegalEntityID: "22222222-2222-4222-8222-222222222222", WorkflowStatus: "PENDING"}
	cancelled := &domain.WorkflowInstance{WorkflowInstanceID: testCancelledID, LegalEntityID: "22222222-2222-4222-8222-222222222222", WorkflowStatus: "CANCELLED"}
	stage := &domain.WorkflowStage{StageOrder: 1, ApproverPrincipalID: "approver-1"}
	approve := func(id string) *http.Request {
		return scopedAs(httptest.NewRequest(http.MethodPost, "/v1/workflows/"+id+"/actions", bytes.NewBufferString(`{"action":"APPROVE"}`)), "approver-1")
	}

	for _, tc := range []struct {
		name   string
		w      *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"400 audit engagement without correlation id", serve(newTestRouter(&stubStore{}),
			scopedAs(httptest.NewRequest(http.MethodPost, "/v1/audit/engagements/", bytes.NewBufferString(validAuditEngagementBody())), "manager-1")),
			400, "missing_correlation_id"},
		{"401 no principal", serve(newTestRouter(&stubStore{}),
			scoped(httptest.NewRequest(http.MethodPost, "/v1/workflows", bytes.NewBufferString(validCreateBody())))), 401, "missing_principal"},
		{"403 not authorized to read", get(routerWithReadMode(&stubStore{findInstance: readInstance()}, &stubAuthz{err: domain.ErrAuthorizationDenied}, ""),
			"/v1/workflows/"+testWorkflowID, "stranger-1"), 403, "authorization_denied"},
		{"404 unknown workflow", serve(newTestRouter(&stubStore{findErr: domain.ErrWorkflowNotFound, currentStageErr: domain.ErrWorkflowNotFound}),
			scopedAs(httptest.NewRequest(http.MethodGet, "/v1/workflows/"+testWorkflowID+"/next-approver", nil), "reader-1")), 404, ""},
		{"409 illegal transition", serve(newTestRouter(&stubStore{findInstance: cancelled, currentStage: stage}), approve(testCancelledID)), 409, ""},
		{"503 authorization unavailable", serve(newTestRouterFull(&stubStore{findInstance: pending, currentStage: stage}, &stubPublisher{},
			&stubAuthz{err: domain.ErrAuthorizationServiceUnavailable}), approve(testWorkflowID)), 503, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := assertProblem(t, tc.w, tc.status, tc.code)
			if _, legacy := p["error"]; legacy {
				t.Fatalf("legacy error member survived: %v", p)
			}
		})
	}
}

// A write-strict envelope refusal is a problem too, not the vendored
// middleware's legacy application/json body. EnvelopeRefusals keeps the
// original members (error, violations) as extension members.
func TestAPI2_EnvelopeRefusalIsAProblem(t *testing.T) {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(apicontract.EnvelopeRefusals)
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeWriteStrict, nil))
	handler.RegisterRoutes(r, handler.New(&stubStore{}, &stubPublisher{}, &stubAuthz{}, &stubDocuments{}, zap.NewNop()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/workflows", bytes.NewBufferString(validCreateBody())))
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusBadRequest {
		t.Fatalf("an envelope-less write: want a refusal, got %d %s", w.Code, w.Body.String())
	}
	assertProblem(t, w, w.Code, "")
}

func assertProblem(t *testing.T, w *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != apicontract.ContentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s: %s", ct, apicontract.ContentTypeProblem, w.Body.String())
	}
	var p map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not JSON: %s", w.Body.String())
	}
	typ, _ := p["type"].(string)
	got, _ := p["code"].(string)
	if got == "" || (code != "" && got != code) || p["status"] != float64(status) || !strings.HasPrefix(typ, apicontract.ProblemBase) {
		t.Fatalf("problem = %v, want code %q", p, code)
	}
	return p
}
