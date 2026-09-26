package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/webhook"
)

type mockWebhookStore struct {
	mu           sync.Mutex
	attempts     map[string]*webhook.AttemptLookupResult
	events       map[string]*ledger.DeliveryEvent
	suppressions map[string]*ledger.EmailSuppression
	dlq          map[string]*webhook.DLQItem
	failNext     error
}

func newMockWebhookStore() *mockWebhookStore {
	return &mockWebhookStore{
		attempts:     make(map[string]*webhook.AttemptLookupResult),
		events:       make(map[string]*ledger.DeliveryEvent),
		suppressions: make(map[string]*ledger.EmailSuppression),
		dlq:          make(map[string]*webhook.DLQItem),
	}
}

func (m *mockWebhookStore) LookupAttemptByProviderMessageID(_ context.Context, id string) (*webhook.AttemptLookupResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext != nil {
		err := m.failNext
		m.failNext = nil
		return nil, err
	}
	res, ok := m.attempts[id]
	if !ok {
		return nil, errors.New("attempt not found")
	}
	return res, nil
}

func (m *mockWebhookStore) RecordDeliveryEventIdempotent(_ context.Context, ev *ledger.DeliveryEvent) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext != nil {
		err := m.failNext
		m.failNext = nil
		return false, err
	}
	key := ev.TenantID + ":" + ev.ProviderAttemptID + ":" + string(ev.EventType)
	if _, ok := m.events[key]; ok {
		return false, nil // Idempotent duplicate
	}
	m.events[key] = ev
	return true, nil
}

func (m *mockWebhookStore) AddSuppression(_ context.Context, supp *ledger.EmailSuppression) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext != nil {
		err := m.failNext
		m.failNext = nil
		return err
	}
	key := supp.TenantID + ":" + supp.RecipientEmail + ":" + supp.SourceStream
	m.suppressions[key] = supp
	return nil
}

func (m *mockWebhookStore) RouteToDLQ(_ context.Context, item *webhook.DLQItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if item.DLQID == "" {
		item.DLQID = uuid.NewString()
	}
	m.dlq[item.DLQID] = item
	return nil
}

func (m *mockWebhookStore) GetDLQItem(_ context.Context, _, dlqID string) (*webhook.DLQItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.dlq[dlqID]
	if !ok {
		return nil, errors.New("dlq item not found")
	}
	return item, nil
}

func (m *mockWebhookStore) UpdateDLQStatus(_ context.Context, _, dlqID string, status webhook.DLQStatus, retryCount int, nextRetryAt *time.Time, errReason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.dlq[dlqID]
	if !ok {
		return errors.New("dlq item not found")
	}
	item.Status = status
	item.RetryCount = retryCount
	item.NextRetryAt = nextRetryAt
	item.ErrorReason = errReason
	return nil
}

func (m *mockWebhookStore) ListRetryableDLQ(_ context.Context, _ int) ([]*webhook.DLQItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []*webhook.DLQItem
	for _, it := range m.dlq {
		if it.IsRetryable && it.Status == webhook.DLQStatusFailed {
			list = append(list, it)
		}
	}
	return list, nil
}

func TestWebhook_ValidIngestion_Delivered(t *testing.T) {
	store := newMockWebhookStore()
	msgID := "<msg-deliv-001@zoikosuite.com>"
	store.attempts[msgID] = &webhook.AttemptLookupResult{
		ProviderAttemptID: "attempt-1",
		MessageIntentID:   "intent-1",
		TenantID:          "tenant-deliv",
		SenderStream:      "TRANSACTIONAL",
		RecipientAddress:  "alice@example.com",
	}

	processor := webhook.NewProcessor(store, zap.NewNop())

	payload := `{
		"event_id": "evt-deliv-001",
		"event_type": "DELIVERED",
		"recipient_email": "alice@example.com",
		"provider_message_id": "<msg-deliv-001@zoikosuite.com>"
	}`

	err := processor.ProcessRawPayload(context.Background(), "smtp", []byte(payload))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(store.events) != 1 {
		t.Fatalf("expected 1 delivery event, got %d", len(store.events))
	}
	if len(store.suppressions) != 0 {
		t.Errorf("delivered event must not create suppressions")
	}
	if len(store.dlq) != 0 {
		t.Errorf("delivered event must not route to dlq")
	}
}

func TestWebhook_HardBounce_SuppressionCreated(t *testing.T) {
	store := newMockWebhookStore()
	msgID := "<msg-bounce-001@zoikosuite.com>"
	store.attempts[msgID] = &webhook.AttemptLookupResult{
		ProviderAttemptID: "attempt-2",
		MessageIntentID:   "intent-2",
		TenantID:          "tenant-bounce",
		SenderStream:      "MARKETING",
		RecipientAddress:  "bounced@example.com",
	}

	processor := webhook.NewProcessor(store, zap.NewNop())

	payload := `{
		"event_id": "evt-bounce-001",
		"event_type": "BOUNCE",
		"bounce_type": "HARD",
		"recipient_email": "bounced@example.com",
		"provider_message_id": "<msg-bounce-001@zoikosuite.com>",
		"diagnostic_code": "550 5.1.1 User unknown"
	}`

	err := processor.ProcessRawPayload(context.Background(), "sendgrid", []byte(payload))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(store.events) != 1 {
		t.Fatalf("expected 1 delivery event, got %d", len(store.events))
	}
	if len(store.suppressions) != 1 {
		t.Fatalf("expected 1 suppression entry created, got %d", len(store.suppressions))
	}

	suppKey := "tenant-bounce:bounced@example.com:ALL"
	supp, ok := store.suppressions[suppKey]
	if !ok {
		t.Fatalf("expected suppression key %s, found none", suppKey)
	}
	if supp.Reason != ledger.SuppressionReasonHardBounce {
		t.Errorf("expected reason HARD_BOUNCE, got %s", supp.Reason)
	}
	if supp.SourceStream != "ALL" {
		t.Errorf("expected source stream ALL, got %s", supp.SourceStream)
	}
}

func TestWebhook_SoftBounce_NoSuppressionCreated(t *testing.T) {
	store := newMockWebhookStore()
	msgID := "<msg-soft-001@zoikosuite.com>"
	store.attempts[msgID] = &webhook.AttemptLookupResult{
		ProviderAttemptID: "attempt-3",
		MessageIntentID:   "intent-3",
		TenantID:          "tenant-soft",
		SenderStream:      "OPERATIONAL",
		RecipientAddress:  "mailboxfull@example.com",
	}

	processor := webhook.NewProcessor(store, zap.NewNop())

	payload := `{
		"event_id": "evt-soft-001",
		"event_type": "BOUNCE",
		"bounce_type": "SOFT",
		"recipient_email": "mailboxfull@example.com",
		"provider_message_id": "<msg-soft-001@zoikosuite.com>",
		"diagnostic_code": "452 4.2.2 Mailbox full"
	}`

	err := processor.ProcessRawPayload(context.Background(), "generic", []byte(payload))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(store.events) != 1 {
		t.Fatalf("expected 1 delivery event recorded for soft bounce, got %d", len(store.events))
	}
	if len(store.suppressions) != 0 {
		t.Fatalf("soft bounce must NEVER create permanent suppression, got %d", len(store.suppressions))
	}
}

func TestWebhook_Complaint_SuppressionCreated(t *testing.T) {
	store := newMockWebhookStore()
	msgID := "<msg-complaint-001@zoikosuite.com>"
	store.attempts[msgID] = &webhook.AttemptLookupResult{
		ProviderAttemptID: "attempt-4",
		MessageIntentID:   "intent-4",
		TenantID:          "tenant-comp",
		SenderStream:      "MARKETING",
		RecipientAddress:  "unhappy@example.com",
	}

	processor := webhook.NewProcessor(store, zap.NewNop())

	payload := `{
		"event_id": "evt-comp-001",
		"event_type": "COMPLAINT",
		"recipient_email": "unhappy@example.com",
		"provider_message_id": "<msg-complaint-001@zoikosuite.com>"
	}`

	err := processor.ProcessRawPayload(context.Background(), "ses", []byte(payload))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	suppKey := "tenant-comp:unhappy@example.com:ALL"
	supp, ok := store.suppressions[suppKey]
	if !ok {
		t.Fatalf("missing expected complaint suppression")
	}
	if supp.Reason != ledger.SuppressionReasonComplaint {
		t.Errorf("expected reason COMPLAINT, got %s", supp.Reason)
	}
	if supp.SourceStream != "ALL" {
		t.Errorf("complaint must suppress ALL streams, got %s", supp.SourceStream)
	}
}

func TestWebhook_Unsubscribe_SuppressionCreated(t *testing.T) {
	store := newMockWebhookStore()
	msgID := "<msg-unsub-001@zoikosuite.com>"
	store.attempts[msgID] = &webhook.AttemptLookupResult{
		ProviderAttemptID: "attempt-5",
		MessageIntentID:   "intent-5",
		TenantID:          "tenant-unsub",
		SenderStream:      "MARKETING",
		RecipientAddress:  "optout@example.com",
	}

	processor := webhook.NewProcessor(store, zap.NewNop())

	payload := `{
		"event_id": "evt-unsub-001",
		"event_type": "UNSUBSCRIBE",
		"recipient_email": "optout@example.com",
		"provider_message_id": "<msg-unsub-001@zoikosuite.com>",
		"source_stream": "MARKETING"
	}`

	err := processor.ProcessRawPayload(context.Background(), "sendgrid", []byte(payload))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	suppKey := "tenant-unsub:optout@example.com:MARKETING"
	supp, ok := store.suppressions[suppKey]
	if !ok {
		t.Fatalf("missing expected unsubscribe suppression")
	}
	if supp.Reason != ledger.SuppressionReasonUnsubscribe {
		t.Errorf("expected reason UNSUBSCRIBE, got %s", supp.Reason)
	}
	if supp.SourceStream != "MARKETING" {
		t.Errorf("unsubscribe must target marketing stream, got %s", supp.SourceStream)
	}
}

func TestWebhook_DuplicateEvent_Idempotent(t *testing.T) {
	store := newMockWebhookStore()
	msgID := "<msg-idemp-001@zoikosuite.com>"
	store.attempts[msgID] = &webhook.AttemptLookupResult{
		ProviderAttemptID: "attempt-6",
		MessageIntentID:   "intent-6",
		TenantID:          "tenant-idemp",
		SenderStream:      "MARKETING",
		RecipientAddress:  "idemp@example.com",
	}

	processor := webhook.NewProcessor(store, zap.NewNop())

	payload := `{
		"event_id": "evt-idemp-001",
		"event_type": "DELIVERED",
		"recipient_email": "idemp@example.com",
		"provider_message_id": "<msg-idemp-001@zoikosuite.com>"
	}`

	// First pass
	if err := processor.ProcessRawPayload(context.Background(), "smtp", []byte(payload)); err != nil {
		t.Fatalf("first delivery pass failed: %v", err)
	}
	// Second duplicate pass
	if err := processor.ProcessRawPayload(context.Background(), "smtp", []byte(payload)); err != nil {
		t.Fatalf("second duplicate pass failed: %v", err)
	}

	if len(store.events) != 1 {
		t.Errorf("duplicate event must be deduplicated, found %d events", len(store.events))
	}
}

func TestWebhook_MalformedPayload_RejectionAndDLQ(t *testing.T) {
	store := newMockWebhookStore()
	processor := webhook.NewProcessor(store, zap.NewNop())

	invalidPayload := []byte(`{not valid json`)
	err := processor.ProcessRawPayload(context.Background(), "ses", invalidPayload)
	if err == nil {
		t.Fatalf("expected error for malformed json")
	}

	if len(store.dlq) != 1 {
		t.Fatalf("malformed payload must route to DLQ, got %d items", len(store.dlq))
	}
	for _, it := range store.dlq {
		if it.IsRetryable {
			t.Errorf("malformed json must not be marked retryable")
		}
		if it.TenantID != "SYSTEM_UNRESOLVED" {
			t.Errorf("malformed payload should have fallback tenant SYSTEM_UNRESOLVED, got %s", it.TenantID)
		}
	}
}

func TestWebhook_UnresolvedTenant_DLQRouting(t *testing.T) {
	store := newMockWebhookStore()
	processor := webhook.NewProcessor(store, zap.NewNop())

	// No matching attempt in store
	payload := `{
		"event_id": "evt-unknown-001",
		"event_type": "BOUNCE",
		"recipient_email": "unknown@example.com",
		"provider_message_id": "<non-existent-msg@zoikosuite.com>"
	}`

	err := processor.ProcessRawPayload(context.Background(), "smtp", []byte(payload))
	if err != nil {
		t.Fatalf("unresolved tenant should be cleanly handled and dead-lettered, got err: %v", err)
	}

	if len(store.dlq) != 1 {
		t.Fatalf("unresolved event must be routed to DLQ, found %d", len(store.dlq))
	}
	for _, it := range store.dlq {
		if !strings.Contains(it.ErrorReason, "unresolved tenant") {
			t.Errorf("expected reason to mention unresolved tenant, got %s", it.ErrorReason)
		}
	}
}

func TestWebhook_RetryableProcessingFailure_DLQ(t *testing.T) {
	store := newMockWebhookStore()
	msgID := "<msg-transient-001@zoikosuite.com>"
	store.attempts[msgID] = &webhook.AttemptLookupResult{
		ProviderAttemptID: "attempt-7",
		MessageIntentID:   "intent-7",
		TenantID:          "tenant-transient",
		SenderStream:      "MARKETING",
		RecipientAddress:  "transient@example.com",
	}
	// Simulate temporary database lock / failure
	store.failNext = errors.New("transient database connection reset")

	processor := webhook.NewProcessor(store, zap.NewNop())

	payload := `{
		"event_id": "evt-transient-001",
		"event_type": "BOUNCE",
		"bounce_type": "HARD",
		"recipient_email": "transient@example.com",
		"provider_message_id": "<msg-transient-001@zoikosuite.com>"
	}`

	err := processor.ProcessRawPayload(context.Background(), "sendgrid", []byte(payload))
	if err == nil {
		t.Fatalf("expected transient error")
	}

	if len(store.dlq) != 1 {
		t.Fatalf("transient failure must route to DLQ, got %d items", len(store.dlq))
	}
	for _, it := range store.dlq {
		if !it.IsRetryable {
			t.Errorf("expected DLQ item to be marked retryable")
		}
		if it.NextRetryAt == nil {
			t.Errorf("expected next_retry_at to be set on retryable failure")
		}
	}
}

func TestWebhook_ReprocessDLQItem_Success(t *testing.T) {
	store := newMockWebhookStore()
	msgID := "<msg-reproc-001@zoikosuite.com>"
	store.attempts[msgID] = &webhook.AttemptLookupResult{
		ProviderAttemptID: "attempt-8",
		MessageIntentID:   "intent-8",
		TenantID:          "tenant-reproc",
		SenderStream:      "MARKETING",
		RecipientAddress:  "reproc@example.com",
	}

	processor := webhook.NewProcessor(store, zap.NewNop())

	dlqID := "dlq-item-123"
	store.dlq[dlqID] = &webhook.DLQItem{
		DLQID:        dlqID,
		TenantID:     "tenant-reproc",
		ProviderName: "sendgrid",
		EventType:    "BOUNCE",
		RawPayload: json.RawMessage(`{
			"event_id": "evt-reproc-001",
			"event_type": "BOUNCE",
			"bounce_type": "HARD",
			"recipient_email": "reproc@example.com",
			"provider_message_id": "<msg-reproc-001@zoikosuite.com>"
		}`),
		ErrorReason: "prior database network failure",
		IsRetryable: true,
		Status:      webhook.DLQStatusFailed,
		ReceivedAt:  time.Now().UTC(),
	}

	err := processor.ReprocessDLQItem(context.Background(), "tenant-reproc", dlqID)
	if err != nil {
		t.Fatalf("reprocess failed: %v", err)
	}

	dlqItem := store.dlq[dlqID]
	if dlqItem.Status != webhook.DLQStatusReprocessed {
		t.Errorf("expected DLQ status REPROCESSED, got %s", dlqItem.Status)
	}
	if len(store.suppressions) != 1 {
		t.Errorf("reprocessed hard bounce should record suppression, found %d", len(store.suppressions))
	}
}

func TestWebhook_HTTPHandler_Routing(t *testing.T) {
	store := newMockWebhookStore()
	msgID := "<msg-http-001@zoikosuite.com>"
	store.attempts[msgID] = &webhook.AttemptLookupResult{
		ProviderAttemptID: "attempt-http",
		MessageIntentID:   "intent-http",
		TenantID:          "tenant-http",
		SenderStream:      "TRANSACTIONAL",
		RecipientAddress:  "http@example.com",
	}

	processor := webhook.NewProcessor(store, zap.NewNop())
	h := webhook.NewHandler(processor, zap.NewNop())

	r := chi.NewRouter()
	h.RegisterRoutes(r)

	// Valid payload
	payload := `{"event_id":"evt-h-1","event_type":"DELIVERED","recipient_email":"http@example.com","provider_message_id":"<msg-http-001@zoikosuite.com>"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/notifications/webhooks/smtp", bytes.NewReader([]byte(payload)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d (%s)", w.Code, w.Body.String())
	}

	// Malformed payload
	badReq := httptest.NewRequest(http.MethodPost, "/v1/notifications/webhooks/smtp", bytes.NewReader([]byte(`{not json`)))
	badW := httptest.NewRecorder()

	r.ServeHTTP(badW, badReq)

	if badW.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for malformed json, got %d", badW.Code)
	}
}

func TestWebhook_ProcessRetryableDLQ_Batch(t *testing.T) {
	store := newMockWebhookStore()
	msgID := "<msg-batch-reproc@zoikosuite.com>"
	store.attempts[msgID] = &webhook.AttemptLookupResult{
		ProviderAttemptID: "attempt-batch",
		MessageIntentID:   "intent-batch",
		TenantID:          "tenant-batch",
		SenderStream:      "MARKETING",
		RecipientAddress:  "batch@example.com",
	}

	processor := webhook.NewProcessor(store, zap.NewNop())

	dlqID := "dlq-batch-1"
	store.dlq[dlqID] = &webhook.DLQItem{
		DLQID:        dlqID,
		TenantID:     "tenant-batch",
		ProviderName: "generic",
		EventType:    "DELIVERED",
		RawPayload: json.RawMessage(`{
			"event_id": "evt-batch-001",
			"event_type": "DELIVERED",
			"recipient_email": "batch@example.com",
			"provider_message_id": "<msg-batch-reproc@zoikosuite.com>"
		}`),
		ErrorReason: "connection timeout",
		IsRetryable: true,
		Status:      webhook.DLQStatusFailed,
		ReceivedAt:  time.Now().UTC(),
	}

	count, err := processor.ProcessRetryableDLQ(context.Background(), 10)
	if err != nil {
		t.Fatalf("unexpected error during batch retry: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 item reprocessed, got %d", count)
	}

	if store.dlq[dlqID].Status != webhook.DLQStatusReprocessed {
		t.Errorf("expected status REPROCESSED, got %s", store.dlq[dlqID].Status)
	}
	if len(store.events) != 1 {
		t.Errorf("expected 1 delivery event recorded, got %d", len(store.events))
	}
}
