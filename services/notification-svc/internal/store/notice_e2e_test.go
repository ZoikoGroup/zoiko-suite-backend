package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/retry"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/webhook"
)

type allowAll struct{}

func (allowAll) CheckAllowed(context.Context, string, string, string) error { return nil }

type fixedResolver struct{}

func (fixedResolver) ResolveEmail(context.Context, string, string, string) (string, error) {
	return "recipient@example.com", nil
}

type mailProvider struct{ sent []domain.Notification }

func (m *mailProvider) Deliver(_ context.Context, n domain.Notification) domain.DeliveryOutcome {
	m.sent = append(m.sent, n)
	return domain.DeliveryOutcome{Delivered: true, ProviderName: "smtp", ProviderResponse: "smtp mx.example.com:25 accepted; message-id=<e2e-notice@example.com>"}
}

func call(t *testing.T, r http.Handler, method, path string, body any, principal string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principal)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	out := map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

// The whole of NCD-05 through the real HTTP handlers, the real store and the real callback
// processor: prepare, dispatch, provider acceptance (not enough), the mail server's own
// acceptance arriving as a callback (enough), the recipient's response, and the bundle.
func TestNotice_EndToEndThroughHandlerStoreAndCallback(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-notice-e2e"
	iv := regulatedIntentVersion(t, s, tenant, "legal.e2e")
	intentID := iv.IntentID

	mail := &mailProvider{}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), tenant)))
		})
	})
	handler.RegisterRoutes(r, handler.New(handler.Deps{Store: s, AuthZ: allowAll{}, Deliverer: mail, Recipient: fixedResolver{},
		Intents: s, Notices: s, Evidence: s, RetryPolicy: retry.DefaultPolicy, Log: zap.NewNop()}))

	// 1. Prepare.
	code, out := call(t, r, http.MethodPost, "/v1/notices/", map[string]any{"intent_id": intentID, "recipient_principal_id": noticeRecipient,
		"subject": "Notice of change", "body": "Your terms change on 1 January.", "policy_ref": "PDC-RULE-9", "ack_requirement": "RECEIPT",
		"deadline_at": time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)}, "sender-1")
	require.Equal(t, http.StatusCreated, code, "%v", out)
	noticeID := out["notice_id"].(string)
	assert.Equal(t, "READY", out["status"])

	// 2. Dispatch. The provider accepts it, which is NOT delivery evidence.
	code, out = call(t, r, http.MethodPost, "/v1/notices/"+noticeID+"/dispatch", nil, "sender-1")
	require.Equal(t, http.StatusAccepted, code, "%v", out)
	require.Len(t, mail.sent, 1)
	assert.Equal(t, "Your terms change on 1 January.", mail.sent[0].Body)
	assert.NotEmpty(t, mail.sent[0].IntentVersionID, "the delivery is pinned to the intent version")
	assert.Equal(t, "DELIVERY_IN_PROGRESS", out["notice"].(map[string]any)["status"], "provider acceptance alone must not evidence delivery")

	// A repeated dispatch does not send again.
	code, _ = call(t, r, http.MethodPost, "/v1/notices/"+noticeID+"/dispatch", nil, "sender-1")
	assert.Equal(t, http.StatusOK, code)
	assert.Len(t, mail.sent, 1)

	// 3. The receiving mail server's answer arrives as a provider callback.
	proc := webhook.NewProcessor(s, zap.NewNop())
	require.NoError(t, proc.ProcessRawPayload(context.Background(), "generic", []byte(
		`{"event_id":"`+uuid.NewString()+`","event_type":"DELIVERED","recipient_email":"recipient@example.com","provider_message_id":"<e2e-notice@example.com>"}`)))
	code, out = call(t, r, http.MethodGet, "/v1/notices/"+noticeID, nil, noticeRecipient)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "ACK_PENDING", out["notice"].(map[string]any)["status"], "evidenced, and waiting for the recipient")

	// 4. An operator cannot respond; the recipient can.
	code, out = call(t, r, http.MethodPost, "/v1/notices/"+noticeID+"/acknowledgement", map[string]any{"action": "ACKNOWLEDGE"}, "operator-9")
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "operator_acknowledgement_refused", out["error_code"])
	code, out = call(t, r, http.MethodPost, "/v1/notices/"+noticeID+"/acknowledgement", map[string]any{"action": "ACKNOWLEDGE", "comment": "understood"}, noticeRecipient)
	require.Equal(t, http.StatusOK, code, "%v", out)
	assert.Equal(t, "ACKNOWLEDGED", out["notice"].(map[string]any)["status"])

	// 5. The bundle holds the whole story, with the legal boundary stated.
	code, out = call(t, r, http.MethodGet, "/v1/notices/"+noticeID+"/evidence", nil, "admin-1")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, out["attempts"], 1)
	assert.Len(t, out["delivery_evidence"], 1)
	assert.Len(t, out["history"], 6, "PREPARED, READY, DELIVERY_IN_PROGRESS, DELIVERY_EVIDENCED, ACK_PENDING, ACKNOWLEDGED")
	assert.NotNil(t, out["acknowledgement"])
	assert.Contains(t, out["notice_text"], "not a finding that service was legally effective")
}
