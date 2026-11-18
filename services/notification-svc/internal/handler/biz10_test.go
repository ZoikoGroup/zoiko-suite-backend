package handler_test

import (
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/retry"

	"github.com/google/uuid"

	"zoiko.io/notification-svc/internal/ledger"
)

// GetIntent used to skip authorization when the intent carried no legal entity, serving the
// intent and its render to any principal in the tenant. It now refuses: an intent that
// cannot be authorized against anything is not served.
func TestGetIntent_NoLegalEntity_IsRefusedNotServed(t *testing.T) {
	tenant := "tenant-biz10"
	ls := newMockLedgerStoreForHandler()
	intentID := uuid.NewString()
	ls.intents[tenant+":"+intentID] = &ledger.MessageIntent{
		MessageIntentID: intentID, TenantID: tenant, LegalEntityID: "",
		RecipientPrincipalID: "usr-1", RecipientEmail: "x@example.com", Channel: "EMAIL",
		CommunicationClass: ledger.ClassS0, TemplateKey: "ZS-IA-001", EventID: "evt-1",
		SourceEventType: "identity.password_reset_requested", DeduplicationKey: "dedup-1", CorrelationID: "corr-1",
		Status: ledger.IntentStatusDelivered, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	r, _ := newLedgerTestRouter(tenant, ls)
	req := httptest.NewRequest(http.MethodGet, "/v1/notifications/intents/"+intentID, nil)
	req.Header.Set("X-Principal-Id", "anyone")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an intent with no legal entity must be refused (403), got %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); len(body) > 0 && (strings.Contains(body, "x@example.com") || strings.Contains(body, "ZS-IA-001")) {
		t.Fatalf("the refusal must not carry the intent: %s", body)
	}

	// An intent that has one is still served.
	okID := uuid.NewString()
	ls.intents[tenant+":"+okID] = &ledger.MessageIntent{
		MessageIntentID: okID, TenantID: tenant, LegalEntityID: "le-1",
		RecipientPrincipalID: "usr-1", RecipientEmail: "x@example.com", Channel: "EMAIL",
		CommunicationClass: ledger.ClassS0, TemplateKey: "ZS-IA-001", EventID: "evt-2",
		SourceEventType: "identity.password_reset_requested", DeduplicationKey: "dedup-2", CorrelationID: "corr-2",
		Status: ledger.IntentStatusDelivered, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/notifications/intents/"+okID, nil)
	req.Header.Set("X-Principal-Id", "anyone")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("an intent with a legal entity must still be served, got %d", rec.Code)
	}
}

// The suppression endpoints used to skip authorization when no authorizer was configured.
// An unset authorizer must refuse, never open the endpoint.
func TestSuppressionAdmin_UnsetAuthorizerFailsClosed(t *testing.T) {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-biz10")))
		})
	})
	handler.RegisterRoutes(r, handler.New(handler.Deps{Store: newStubStore(), Deliverer: &stubDeliverer{delivered: true},
		Recipient: &stubResolver{email: "r@example.com"}, RetryPolicy: retry.DefaultPolicy, Log: zap.NewNop()}))

	rr := doReq(r, http.MethodGet, "/v1/notifications/suppression/", nil, "anyone")
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "authorization not configured") {
		t.Fatalf("an unset authorizer must refuse the suppression list (503), got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doReq(r, http.MethodPost, "/v1/notifications/suppression/", map[string]any{"address": "a@example.com"}, "anyone")
	if rr.Code != http.StatusServiceUnavailable && rr.Code != http.StatusBadRequest {
		t.Fatalf("an unset authorizer must not let a suppression through, got %d: %s", rr.Code, rr.Body.String())
	}
}
