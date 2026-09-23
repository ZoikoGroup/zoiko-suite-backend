package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/retry"
	"zoiko.io/notification-svc/internal/store"
)

type mockLedgerStoreForHandler struct {
	intents map[string]*ledger.MessageIntent // key: tenantID + ":" + id
	renders map[string]*ledger.MessageRender
}

func newMockLedgerStoreForHandler() *mockLedgerStoreForHandler {
	return &mockLedgerStoreForHandler{
		intents: make(map[string]*ledger.MessageIntent),
		renders: make(map[string]*ledger.MessageRender),
	}
}

func (m *mockLedgerStoreForHandler) CreateMessageIntent(ctx context.Context, intent *ledger.MessageIntent) (bool, *ledger.MessageIntent, error) {
	key := intent.TenantID + ":" + intent.MessageIntentID
	m.intents[key] = intent
	return true, intent, nil
}

func (m *mockLedgerStoreForHandler) RecordMessageRender(ctx context.Context, render *ledger.MessageRender) error {
	m.renders[render.TenantID+":"+render.MessageIntentID] = render
	return nil
}

func (m *mockLedgerStoreForHandler) RecordDeliveryAttempt(ctx context.Context, attempt *ledger.DeliveryAttempt) error {
	return nil
}

func (m *mockLedgerStoreForHandler) RecordDeliveryEvent(ctx context.Context, event *ledger.DeliveryEvent) error {
	return nil
}

func (m *mockLedgerStoreForHandler) UpdateIntentStatus(ctx context.Context, tenantID, intentID string, status ledger.IntentStatus, failureReason *string) error {
	if intent, ok := m.intents[tenantID+":"+intentID]; ok {
		intent.Status = status
		intent.FailureReason = failureReason
		return nil
	}
	return store.ErrIntentNotFound
}

func (m *mockLedgerStoreForHandler) GetMessageIntent(ctx context.Context, tenantID, intentID string) (*ledger.MessageIntent, error) {
	if intent, ok := m.intents[tenantID+":"+intentID]; ok {
		return intent, nil
	}
	return nil, store.ErrIntentNotFound
}

func (m *mockLedgerStoreForHandler) GetRenderByIntent(ctx context.Context, tenantID, intentID string) (*ledger.MessageRender, error) {
	if render, ok := m.renders[tenantID+":"+intentID]; ok {
		return render, nil
	}
	return nil, store.ErrRenderNotFound
}

func newLedgerTestRouter(tenantID string, ls *mockLedgerStoreForHandler) (chi.Router, *ledger.Orchestrator) {
	compiler := ledger.NewCompiler()
	for _, seed := range ledger.DefaultSeedDefinitions() {
		_ = compiler.Register(seed)
	}
	killSwitch := ledger.NewKillSwitchManager(zap.NewNop())
	deliverer := &mockDeliverer{
		outcome: domain.DeliveryOutcome{
			Delivered:        true,
			ProviderResponse: "250 2.0.0 OK",
		},
	}
	res := &stubResolver{email: "recipient@example.com"}
	orc := ledger.NewOrchestrator(ls, compiler, killSwitch, deliverer, res, zap.NewNop())

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if tenantID != "" {
				req = req.WithContext(middleware.WithTenant(req.Context(), tenantID))
			}
			next.ServeHTTP(w, req)
		})
	})

	h := handler.New(handler.Deps{
		Store:        newStubStore(),
		Publisher:    &stubPublisher{},
		AuthZ:        &stubAuthZ{},
		Deliverer:    deliverer,
		Recipient:    res,
		RetryPolicy:  retry.DefaultPolicy,
		Orchestrator: orc,
		LedgerStore:  ls,
		Log:          zap.NewNop(),
	})
	handler.RegisterRoutes(r, h)
	return r, orc
}

type mockDeliverer struct {
	outcome domain.DeliveryOutcome
}

func (d *mockDeliverer) Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome {
	return d.outcome
}

func TestHandler_IngestEvent_Success(t *testing.T) {
	tenantID := "tenant-test"
	ls := newMockLedgerStoreForHandler()
	r, _ := newLedgerTestRouter(tenantID, ls)

	payload := map[string]any{
		"event_id":               "evt-100",
		"event_type":             "identity.password_reset_requested",
		"recipient_principal_id": "usr-100",
		"legal_entity_id":        "le-us",
		"template_key":           "ZS-IA-001",
		"correlation_id":         "corr-100",
		"variables": map[string]string{
			"recipient.first_name":           "Bob",
			"recipient.email_masked":         "b***@example.com",
			"links.action_url":               "https://example.com/verify",
			"security.link_expires_at_local": "15 minutes",
			"message.reference":              "REF-100",
		},
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/notifications/events/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-1")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	var res ledger.OrchestrationResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if res.MessageIntentID == "" {
		t.Fatalf("expected non-empty message_intent_id")
	}
	if res.Status != ledger.IntentStatusDispatched {
		t.Fatalf("expected status DISPATCHED, got %s", res.Status)
	}
}

func TestHandler_IngestEvent_MissingFields(t *testing.T) {
	tenantID := "tenant-test"
	ls := newMockLedgerStoreForHandler()
	r, _ := newLedgerTestRouter(tenantID, ls)

	payload := map[string]any{
		"event_id": "evt-incomplete",
		// missing event_type, recipient_principal_id, etc.
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/notifications/events/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-1")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_IngestEvent_MissingTenant_Rejected(t *testing.T) {
	ls := newMockLedgerStoreForHandler()
	r, _ := newLedgerTestRouter("", ls) // No tenant installed

	payload := map[string]any{
		"event_id":               "evt-100",
		"event_type":             "identity.password_reset_requested",
		"recipient_principal_id": "usr-100",
		"legal_entity_id":        "le-us",
		"template_key":           "ZS-IA-001",
		"correlation_id":         "corr-100",
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/notifications/events/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-1")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for missing tenant, got %d", rec.Code)
	}
}

func TestHandler_GetIntent_TenantIsolation(t *testing.T) {
	tenantA := "tenant-alpha"
	tenantB := "tenant-beta"

	ls := newMockLedgerStoreForHandler()
	intentID := uuid.NewString()

	// Seed Tenant A intent
	ls.intents[tenantA+":"+intentID] = &ledger.MessageIntent{
		MessageIntentID:      intentID,
		TenantID:             tenantA,
		LegalEntityID:        "le-alpha",
		RecipientPrincipalID: "usr-alpha",
		RecipientEmail:       "alpha@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassS0,
		TemplateKey:          "ZS-IA-001",
		EventID:              "evt-alpha",
		SourceEventType:      "identity.password_reset_requested",
		DeduplicationKey:     "dedup-alpha",
		CorrelationID:        "corr-alpha",
		Status:               ledger.IntentStatusDelivered,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}

	// 1. Tenant A fetches its own intent: succeeds
	rA, _ := newLedgerTestRouter(tenantA, ls)
	reqA := httptest.NewRequest(http.MethodGet, "/v1/notifications/intents/"+intentID, nil)
	reqA.Header.Set("X-Principal-Id", "usr-alpha")
	recA := httptest.NewRecorder()
	rA.ServeHTTP(recA, reqA)

	if recA.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for Tenant A, got %d: %s", recA.Code, recA.Body.String())
	}

	// 2. Tenant B fetches Tenant A's intent ID: 404 Not Found (tenant isolated)
	rB, _ := newLedgerTestRouter(tenantB, ls)
	reqB := httptest.NewRequest(http.MethodGet, "/v1/notifications/intents/"+intentID, nil)
	reqB.Header.Set("X-Principal-Id", "usr-beta")
	recB := httptest.NewRecorder()
	rB.ServeHTTP(recB, reqB)

	if recB.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for cross-tenant read by Tenant B, got %d: %s", recB.Code, recB.Body.String())
	}
}

// Suppress unused error import if any
var _ = errors.New
