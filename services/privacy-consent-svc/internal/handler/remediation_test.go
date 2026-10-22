package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/privacy-consent-svc/internal/domain"
	"zoiko.io/privacy-consent-svc/internal/handler"
	"zoiko.io/privacy-consent-svc/internal/middleware"
)

// ── Gap 6: body validation never precedes authentication / authorization ────

// mutatingRoutes lists every mutating route with a body the handler would
// REJECT (400/422) if it validated before authenticating.
func mutatingRoutes() []struct{ name, path string } {
	return []struct{ name, path string }{
		{"create notice", "/privacy/notices"},
		{"create notice version", "/privacy/notices/n1/versions"},
		{"approve", "/privacy/notices/n1/approve"},
		{"publish", "/privacy/notices/n1/publish"},
		{"withdraw notice", "/privacy/notices/n1/withdraw"},
		{"presentation latest", "/privacy/notices/n1/presentation-receipts"},
		{"presentation explicit", "/privacy/notices/n1/versions/v1/presentation-receipts"},
		{"record consent", "/privacy/consents"},
		{"withdraw consent", "/privacy/consents/r1/withdraw"},
		{"invalidate consent", "/privacy/consents/r1/invalidate"},
		{"set preference", "/privacy/preferences"},
	}
}

func rawDo(r http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func rawPost(r http.Handler, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	return rawDo(r, http.MethodPost, path, body, headers)
}

func TestValidationNeverPrecedesAuthentication(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	for _, rt := range mutatingRoutes() {
		// Malformed body, no principal -> 401, never 400/422.
		w := rawPost(r, rt.path, "{not json", map[string]string{"X-Tenant-Id": testTenant, "X-Correlation-ID": "c", "Idempotency-Key": "k-" + rt.name})
		if w.Code != http.StatusUnauthorized || errorCodeOf(t, w) != domain.ErrCodeAuthenticationFailed {
			t.Fatalf("%s: unauthenticated malformed body must be 401 AUTHENTICATION_FAILED, got %d %s", rt.name, w.Code, w.Body.String())
		}
		// Principal but no tenant -> 401 TENANT_SCOPE_REQUIRED (principal is checked first).
		w = rawPost(r, rt.path, "{not json", map[string]string{"X-Principal-Id": "p", "X-Correlation-ID": "c", "Idempotency-Key": "k-" + rt.name})
		if w.Code != http.StatusUnauthorized || errorCodeOf(t, w) != domain.ErrCodeTenantScopeRequired {
			t.Fatalf("%s: missing tenant must be 401 TENANT_SCOPE_REQUIRED, got %d %s", rt.name, w.Code, w.Body.String())
		}
		// Principal + tenant, no correlation id -> 400 CORRELATION_ID_REQUIRED before authorization.
		deny := newTestRouter(newStubStore(), &stubAuthz{deny: true}, grantingPurposeRegistry())
		w = rawPost(deny, rt.path, "{not json", map[string]string{"X-Principal-Id": "p", "X-Tenant-Id": testTenant, "Idempotency-Key": "k-" + rt.name})
		if w.Code != http.StatusBadRequest || errorCodeOf(t, w) != domain.ErrCodeCorrelationRequired {
			t.Fatalf("%s: correlation id is checked before authorization, got %d %s", rt.name, w.Code, w.Body.String())
		}
		// Unauthorized + malformed body -> 403, never 400/422.
		w = rawPost(deny, rt.path, "{not json", map[string]string{"X-Principal-Id": "p", "X-Tenant-Id": testTenant, "X-Correlation-ID": "c", "Idempotency-Key": "k-" + rt.name})
		if w.Code != http.StatusForbidden || errorCodeOf(t, w) != domain.ErrCodeAuthorizationDenied {
			t.Fatalf("%s: unauthorized malformed body must be 403, got %d %s", rt.name, w.Code, w.Body.String())
		}
		// Authorized but the key is missing -> 400 IDEMPOTENCY_KEY_REQUIRED before the body is parsed.
		w = rawPost(r, rt.path, "{not json", map[string]string{"X-Principal-Id": "p", "X-Tenant-Id": testTenant, "X-Correlation-ID": "c"})
		if w.Code != http.StatusBadRequest || errorCodeOf(t, w) != domain.ErrCodeIdempotencyRequired {
			t.Fatalf("%s: missing key must be 400 IDEMPOTENCY_KEY_REQUIRED, got %d %s", rt.name, w.Code, w.Body.String())
		}
		// Fully authenticated and authorized: NOW the body is validated.
		w = rawPost(r, rt.path, "{not json", map[string]string{"X-Principal-Id": "p", "X-Tenant-Id": testTenant, "X-Correlation-ID": "c", "Idempotency-Key": "kk-" + rt.name})
		switch rt.name {
		case "approve", "publish", "withdraw notice": // these routes parse no body; the notice is resolved only now
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s: once authorized the notice is resolved (404 for an unknown id), got %d %s", rt.name, w.Code, w.Body.String())
			}
		case "withdraw consent", "invalidate consent":
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: a malformed body must be 400 once authorized, got %d %s", rt.name, w.Code, w.Body.String())
			}
		default:
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: a malformed body must be 400 once authorized, got %d %s", rt.name, w.Code, w.Body.String())
			}
		}
	}
}

func TestReadsValidateParametersOnlyAfterAuthentication(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	deny := newTestRouter(newStubStore(), &stubAuthz{deny: true}, grantingPurposeRegistry())
	for _, path := range []string{"/privacy/consents", "/privacy/preferences"} { // required query parameters missing
		w := rawDo(r, http.MethodGet, path, "", map[string]string{"X-Tenant-Id": testTenant})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without a principal must be 401, got %d", path, w.Code)
		}
		w = rawDo(r, http.MethodGet, path, "", map[string]string{"X-Principal-Id": "p"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without a tenant must be 401, got %d", path, w.Code)
		}
		w = rawDo(deny, http.MethodGet, path, "", map[string]string{"X-Principal-Id": "p", "X-Tenant-Id": testTenant})
		if w.Code != http.StatusForbidden {
			t.Fatalf("GET %s unauthorized must be 403 (not 400), got %d", path, w.Code)
		}
		w = rawDo(r, http.MethodGet, path, "", map[string]string{"X-Principal-Id": "p", "X-Tenant-Id": testTenant})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("GET %s authorized with missing parameters must be 400, got %d", path, w.Code)
		}
	}
}

// ── Gap 6: §18 presentation-receipt response ────────────────────────────────

func TestPresentationReceipt_CarriesTheSection18ReceiptID(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	v := createPublishedNotice(t, r)
	w := doRequest(r, http.MethodPost, "/privacy/notices/"+v.NoticeID+"/versions/"+v.NoticeVersionID+"/presentation-receipts",
		domain.RecordPresentationRequest{SubjectRef: "s1", Channel: "WEB", SessionRef: "sess-1", TemplateVersion: "t-3"}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	m := decodeMap(t, w)
	if m["receipt_id"] == nil || m["receipt_id"] != m["presentation_receipt_id"] {
		t.Fatalf("receipt_id must equal presentation_receipt_id: %v", m)
	}
	for _, f := range []string{"subject_ref", "session_ref", "notice_version_id", "created_at", "channel", "template_version"} {
		if m[f] == nil {
			t.Fatalf("§18 member %s missing from the presentation receipt: %v", f, m)
		}
	}
}

// ── Gap 3: idempotency ───────────────────────────────────────────────────────

func TestIdempotency_ScopedByPrincipal(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	hdr := map[string]string{"Idempotency-Key": "shared"}
	body := domain.SetPreferenceRequest{SubjectRef: "s", ChannelOrPurpose: "email", Value: "ENABLED", Source: "UI"}
	a := doRequestWithHeaders(r, http.MethodPost, "/privacy/preferences", body, testTenant, "principal-A", hdr)
	b := doRequestWithHeaders(r, http.MethodPost, "/privacy/preferences", body, testTenant, "principal-B", hdr)
	if a.Code != http.StatusCreated || b.Code != http.StatusCreated || b.Header().Get("Idempotency-Replay") == "true" || len(st.preferences) != 2 {
		t.Fatalf("principal B must not replay principal A's response: %d %d replay=%q prefs=%d", a.Code, b.Code, b.Header().Get("Idempotency-Replay"), len(st.preferences))
	}
}

func TestIdempotency_BoundToMethodPathAndBody(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	hdr := map[string]string{"Idempotency-Key": "k-route"}
	w1 := doRequestWithHeaders(r, http.MethodPost, "/privacy/preferences", domain.SetPreferenceRequest{SubjectRef: "s", ChannelOrPurpose: "email", Value: "ENABLED", Source: "UI"}, testTenant, "p1", hdr)
	if w1.Code != http.StatusCreated {
		t.Fatalf("got %d", w1.Code)
	}
	// Same key, same principal, a different operation: conflict, never a replay of the wrong answer.
	w2 := doRequestWithHeaders(r, http.MethodPost, "/privacy/notices", domain.CreateNoticeRequest{Locale: "en", Audience: "CUSTOMER", ContentHash: "h"}, testTenant, "p1", hdr)
	if w2.Code != http.StatusConflict || errorCodeOf(t, w2) != domain.ErrCodeIdempotencyMismatch {
		t.Fatalf("same key on a different operation must be 409, got %d %s", w2.Code, w2.Body.String())
	}
}

func TestIdempotency_FailedOperationStoresNothing_RetryExecutes(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	hdr := map[string]string{"Idempotency-Key": "k-fail"}
	// Withdrawing a receipt that does not exist is a 404; the key must stay usable.
	w := doRequestWithHeaders(r, http.MethodPost, "/privacy/consents/does-not-exist/withdraw", domain.WithdrawConsentRequest{Channel: "WEB"}, testTenant, "p1", hdr)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d", w.Code)
	}
	if len(st.idempotency) != 0 {
		t.Fatal("a refused operation must not consume the idempotency key")
	}
}

// ── Gap 2/5: the registry call carries the caller's identity ────────────────

type capturingPurposes struct{ principal, tenant string }

func (c *capturingPurposes) IsPublished(_ context.Context, tenant, principal, _ string) (bool, error) {
	c.tenant, c.principal = tenant, principal
	return true, nil
}

func TestRecordConsent_ForwardsCallerIdentityToTheRegistry(t *testing.T) {
	st := newStubStore()
	cp := &capturingPurposes{}
	h := handler.New(st, &stubAuthz{}, cp, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	w := doRequestWithHeaders(r, http.MethodPost, "/privacy/consents", grantBody("s-id"), testTenant, "caller-42", nil)
	if w.Code != http.StatusCreated || cp.principal != "caller-42" || cp.tenant != testTenant {
		t.Fatalf("expected the caller's identity to reach the registry, got %d tenant=%q principal=%q (%s)", w.Code, cp.tenant, cp.principal, w.Body.String())
	}
}
