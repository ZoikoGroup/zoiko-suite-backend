package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	svcenvelope "zoiko.io/authorization-svc/internal/envelope"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// newEnvelopeRouter is newTestRouter with the canonical input-contract
// middleware actually mounted, in the given enforcement mode. The other handler
// tests deliberately omit it and send no headers; these tests exist to exercise
// the middleware/route interaction that main.go wires, so they must include it —
// and they use handler.EnvelopePolicy rather than a local copy of it, or they
// would prove nothing about what runs in production.
func newEnvelopeRouter(s *stubStore, mode svcenvelope.Mode) chi.Router {
	r := chi.NewRouter()
	r.Use(svcenvelope.MiddlewareWithMode(handler.EnvelopePolicy(), mode, nil))
	h := handler.New(s, &stubPublisher{}, &stubValidator{},
		siem.New("", "authorization-svc", zap.NewNop()), "platform-scope-entity", zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func grantingStore() *stubStore {
	return &stubStore{
		rbacActions: []string{"PAYMENT_APPROVE"},
		rbacBasis:   "rbac:role=FINANCE_APPROVER",
	}
}

const authorizeBody = `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE"}`

func postAuthorize(t *testing.T, r chi.Router, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(authorizeBody))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// fullEnvelope is what a conformant caller sends — policy-svc's shape, which
// was measured at 200 while 86 others were refused.
func fullEnvelope() map[string]string {
	return map[string]string{
		"Content-Type":                   "application/json",
		svcenvelope.HeaderTenantID:       "t-1",
		svcenvelope.HeaderActorSubjectID: "p-caller",
		svcenvelope.HeaderRequestID:      "req-1",
		svcenvelope.HeaderCorrelationID:  "corr-1",
		svcenvelope.HeaderSourceChannel:  string(svcenvelope.ChannelSystem),
		svcenvelope.HeaderLegalEntityID:  "le-1",
		svcenvelope.HeaderIdempotencyKey: "req-1:PAYMENT_APPROVE",
	}
}

// ── the defect this fixes ─────────────────────────────────────────────────────

// TestEnvelope_Authorize_NoEnvelope_AdmittedUnderWriteStrict is the regression
// test for tracker 82i: the evaluation endpoint used to answer 401
// envelope_incomplete to a caller that sent no envelope, which was ~86 of its
// 111 callers.
func TestEnvelope_Authorize_NoEnvelope_AdmittedUnderWriteStrict(t *testing.T) {
	r := newEnvelopeRouter(grantingStore(), svcenvelope.ModeWriteStrict)

	w := postAuthorize(t, r, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out["decision_outcome"] != "GRANTED" {
		t.Fatalf("expected GRANTED, got %v", out["decision_outcome"])
	}
	// Admitted, but not silently: the response says it is out of contract, and
	// the reporter logs it. That flag is what makes the remaining migration
	// measurable instead of forgotten.
	if got := w.Header().Get("X-Envelope-Contract"); got != "violated" {
		t.Errorf("expected X-Envelope-Contract: violated, got %q", got)
	}
}

// TestEnvelope_Authorize_ObligationsSvcHeaders_Admitted sends exactly what
// obligations-svc sends in code, which the second pass measured at 401.
func TestEnvelope_Authorize_ObligationsSvcHeaders_Admitted(t *testing.T) {
	r := newEnvelopeRouter(grantingStore(), svcenvelope.ModeWriteStrict)

	w := postAuthorize(t, r, map[string]string{
		"Content-Type":                  "application/json",
		svcenvelope.HeaderCorrelationID: "corr-1",
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvelope_Authorize_ConformantCaller_Unflagged — a caller that already
// forwards the envelope is unaffected and is not marked as violating.
func TestEnvelope_Authorize_ConformantCaller_Unflagged(t *testing.T) {
	r := newEnvelopeRouter(grantingStore(), svcenvelope.ModeWriteStrict)

	w := postAuthorize(t, r, fullEnvelope())

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Envelope-Contract"); got != "" {
		t.Errorf("conformant caller should not be flagged, got %q", got)
	}
}

// ── the control that must NOT have been weakened ──────────────────────────────

// TestEnvelope_AdminWrites_NoEnvelope_StillRefused is the other half of the
// fix. The relaxation is scoped to one route; every admin write on the
// platform's own authorization engine stays strictly enforced.
func TestEnvelope_AdminWrites_NoEnvelope_StillRefused(t *testing.T) {
	adminWrites := []string{
		"/v1/admin/roles",
		"/v1/admin/roles/r-1/retire",
		"/v1/admin/roles/r-1/permission-bundles",
		"/v1/admin/permission-bundles/pb-1/retire",
		"/v1/admin/role-assignments",
		"/v1/admin/role-assignments/a-1/revoke",
		"/v1/admin/delegated-authorities",
		"/v1/admin/delegated-authorities/d-1/revoke",
		"/v1/admin/sod-rules",
		"/v1/admin/sod-rules/s-1/retire",
		"/v1/admin/abac-rules",
		"/v1/admin/abac-rules/ab-1/retire",
	}

	for _, path := range adminWrites {
		t.Run(path, func(t *testing.T) {
			r := newEnvelopeRouter(grantingStore(), svcenvelope.ModeWriteStrict)

			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 envelope refusal, got %d: %s", w.Code, w.Body.String())
			}
			var out struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if out.Error != "envelope_incomplete" {
				t.Errorf("expected envelope_incomplete, got %q", out.Error)
			}
		})
	}
}

// ── the end state ─────────────────────────────────────────────────────────────

// TestEnvelope_Authorize_StrictMode_StillRefused proves the override is not a
// permanent hole: once the callers are migrated and enforcement moves to
// strict, an envelope-less evaluation call is refused again.
func TestEnvelope_Authorize_StrictMode_StillRefused(t *testing.T) {
	r := newEnvelopeRouter(grantingStore(), svcenvelope.ModeStrict)

	w := postAuthorize(t, r, nil)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 under strict, got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvelope_Authorize_StrictMode_NoIdempotencyKeyDemanded is what the
// override actually changes about the doctrinal end state: under strict, the
// evaluation endpoint requires the five unconditional §4 fields and no longer
// demands an Idempotency-Key or X-Legal-Entity-Id, neither of which an
// evaluation has anything to do with.
func TestEnvelope_Authorize_StrictMode_NoIdempotencyKeyDemanded(t *testing.T) {
	r := newEnvelopeRouter(grantingStore(), svcenvelope.ModeStrict)

	w := postAuthorize(t, r, map[string]string{
		"Content-Type":                   "application/json",
		svcenvelope.HeaderTenantID:       "t-1",
		svcenvelope.HeaderActorSubjectID: "p-caller",
		svcenvelope.HeaderRequestID:      "req-1",
		svcenvelope.HeaderCorrelationID:  "corr-1",
		svcenvelope.HeaderSourceChannel:  string(svcenvelope.ChannelSystem),
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvelope_Authorize_StrictMode_TenantStillRequired — the unconditional
// fields are still unconditional. A caller that omits tenant scope is refused
// under strict even on the evaluation endpoint.
func TestEnvelope_Authorize_StrictMode_TenantStillRequired(t *testing.T) {
	r := newEnvelopeRouter(grantingStore(), svcenvelope.ModeStrict)

	h := fullEnvelope()
	delete(h, svcenvelope.HeaderTenantID)
	w := postAuthorize(t, r, h)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing tenant under strict, got %d: %s", w.Code, w.Body.String())
	}
}

// ── the classifier itself ─────────────────────────────────────────────────────

func TestMaterialWrite_Classification(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, handler.AuthorizePath, false},
		{http.MethodGet, handler.AuthorizePath, false},
		{http.MethodPost, "/v1/admin/roles", true},
		{http.MethodPost, "/v1/admin/sod-rules", true},
		{http.MethodPost, "/v1/admin/abac-rules", true},
		{http.MethodPut, "/v1/admin/roles", true},
		{http.MethodDelete, "/v1/admin/roles", true},
		{http.MethodGet, "/v1/admin/roles", false},
		{http.MethodHead, "/v1/admin/roles", false},
		{http.MethodOptions, "/v1/admin/roles", false},
		{http.MethodGet, "/v1/access-decisions/ad-1", false},
		{http.MethodGet, handler.AccessDecisionsPath, false},
		// Not the evaluation endpoint, despite the shared prefix.
		{http.MethodPost, "/v1/authorize/something", true},

		// The three §8.3 validation routes. All POSTs, because each takes a
		// body, and all non-writes, because none of them writes anything at
		// all — not even the decision artifact /v1/authorize writes. A route
		// missing from handler's evaluatePaths would be classified as a
		// material write, and under write-strict its callers would be refused
		// 401 for want of an Idempotency-Key on a question: exactly the outage
		// tracker 82i recorded, which went undiagnosed for three passes.
		{http.MethodPost, handler.EntityScopeValidatePath, false},
		{http.MethodPost, handler.SoDValidatePath, false},
		{http.MethodPost, handler.DelegatedAccessEvaluatePath, false},
		// Sub-paths of the validation routes are not the validation routes,
		// same as /v1/authorize/something.
		{http.MethodPost, handler.SoDValidatePath + "/extra", true},
	}

	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		if got := handler.MaterialWrite(req); got != c.want {
			t.Errorf("MaterialWrite(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// The three §8.3 validation routes reach their handlers with no envelope at
// all, under the default enforcement mode.
//
// This is the test that would have caught tracker 82i three passes earlier if
// it had existed for /v1/authorize: the other handler tests deliberately mount
// no middleware and send no headers, so none of them can see a refusal that
// happens one layer up. Every route added to the service needs one of these or
// its callers discover the classification in production.
//
// They are still authenticated — the handler itself requires X-Principal-Id and
// X-Tenant-Id — so these send those two and nothing else, which is the shape a
// non-migrated caller has.
func TestEnvelope_ValidationRoutes_NoEnvelope_AdmittedUnderWriteStrict(t *testing.T) {
	cases := []struct{ path, body string }{
		{handler.EntityScopeValidatePath, `{"principal_id":"p-1","legal_entity_ids":["le-1"]}`},
		{handler.SoDValidatePath, `{"candidate_actions":["PAYMENT_APPROVE"]}`},
		{handler.DelegatedAccessEvaluatePath, `{"principal_id":"p-1","legal_entity_id":"le-1"}`},
	}

	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			r := newEnvelopeRouter(grantingStore(), svcenvelope.ModeWriteStrict)

			req := httptest.NewRequest(http.MethodPost, c.path, bytes.NewBufferString(c.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Principal-Id", "p-caller")
			req.Header.Set("X-Tenant-Id", "t-1")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			// Admitted is not the same as silent: the violation is flagged on
			// the response and named by the reporter, which is what makes the
			// migration list generated rather than swept for.
			if got := w.Header().Get("X-Envelope-Contract"); got != "violated" {
				t.Errorf("X-Envelope-Contract = %q, want violated — an admitted violation must still be visible", got)
			}
		})
	}
}

// And under strict they are refused again, with no code change — the same
// property that makes the /v1/authorize override a migration state rather than
// a permanent exemption.
//
// The refusal STATUS depends on which fields are missing, and both cases are
// asserted because the distinction is easy to get wrong when reading a test:
// envelope.StatusFor answers 401 only when tenant_id or actor_subject_id is
// among the violations, and 400 otherwise. So a caller sending no headers at
// all is unauthorized, while one that identifies itself but omits the tracing
// fields is a bad request. Asserting 401 for both would pass today only by
// accident of which header a test happened to send.
func TestEnvelope_ValidationRoutes_StrictMode_StillRefused(t *testing.T) {
	paths := []string{
		handler.EntityScopeValidatePath,
		handler.SoDValidatePath,
		handler.DelegatedAccessEvaluatePath,
	}

	for _, path := range paths {
		t.Run(path+" no headers at all", func(t *testing.T) {
			r := newEnvelopeRouter(grantingStore(), svcenvelope.ModeStrict)

			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			// 401: tenant_id and actor_subject_id are both missing.
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 under strict with no identity, got %d: %s", w.Code, w.Body.String())
			}
		})

		t.Run(path+" identity but no trace", func(t *testing.T) {
			r := newEnvelopeRouter(grantingStore(), svcenvelope.ModeStrict)

			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
			req.Header.Set(svcenvelope.HeaderActorSubjectID, "p-caller")
			req.Header.Set(svcenvelope.HeaderTenantID, "t-1")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			// 400: identity resolved, request_id / correlation_id /
			// source_channel did not.
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 under strict with identity but no trace, got %d: %s", w.Code, w.Body.String())
			}
			var out struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if out.Error != "envelope_incomplete" {
				t.Errorf("expected envelope_incomplete, got %q", out.Error)
			}
		})
	}
}

// The audit read is a GET, so it was never at risk from the write
// classification — but it IS a new route on the service, and the point of the
// test above is that new routes get checked rather than assumed.
func TestEnvelope_ListAccessDecisions_NoEnvelope_Admitted(t *testing.T) {
	r := newEnvelopeRouter(emptyPageStore(), svcenvelope.ModeWriteStrict)

	req := httptest.NewRequest(http.MethodGet, handler.AccessDecisionsPath, nil)
	req.Header.Set("X-Principal-Id", "p-auditor")
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
