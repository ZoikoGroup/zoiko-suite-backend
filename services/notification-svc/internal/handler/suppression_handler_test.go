package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/retry"
)

// ── stubSuppressionStore ──────────────────────────────────────────────────────

type stubSuppressionStore struct {
	suppressions []*ledger.EmailSuppression
	addCalls     int
	removeCalls  int
}

func (s *stubSuppressionStore) AddSuppression(_ context.Context, supp *ledger.EmailSuppression) error {
	s.addCalls++
	// Idempotent: update if already exists
	for _, existing := range s.suppressions {
		if existing.TenantID == supp.TenantID &&
			existing.RecipientEmail == supp.RecipientEmail &&
			existing.SourceStream == supp.SourceStream {
			existing.Reason = supp.Reason
			existing.ProviderName = supp.ProviderName
			return nil
		}
	}
	s.suppressions = append(s.suppressions, supp)
	return nil
}

func (s *stubSuppressionStore) IsEmailSuppressed(_ context.Context, tenantID, recipientEmail string, _ ledger.SenderStream, _ ledger.CommunicationClass) (bool, string, error) {
	for _, supp := range s.suppressions {
		if supp.TenantID == tenantID && supp.RecipientEmail == recipientEmail {
			return true, string(supp.Reason), nil
		}
	}
	return false, "", nil
}

func (s *stubSuppressionStore) RemoveSuppression(_ context.Context, tenantID, recipientEmail, stream string) error {
	s.removeCalls++
	out := s.suppressions[:0]
	for _, supp := range s.suppressions {
		if !(supp.TenantID == tenantID && supp.RecipientEmail == recipientEmail && supp.SourceStream == stream) {
			out = append(out, supp)
		}
	}
	s.suppressions = out
	return nil
}

func (s *stubSuppressionStore) ListSuppressions(_ context.Context, tenantID string, limit, _ int) ([]*ledger.EmailSuppression, error) {
	var out []*ledger.EmailSuppression
	for _, supp := range s.suppressions {
		if supp.TenantID == tenantID {
			out = append(out, supp)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ── test helpers ──────────────────────────────────────────────────────────────

func newSuppressionHandler(t *testing.T, ss *stubSuppressionStore) http.Handler {
	t.Helper()
	h := handler.New(handler.Deps{
		Store:        &stubStore{byID: make(map[string]*domain.Notification), templates: make(map[string]*domain.TemplateDefinition), versions: make(map[string]*domain.TemplateVersion)},
		Publisher:    &stubPublisher{},
		AuthZ:        &stubAuthZ{},
		Deliverer:    &stubDeliverer{delivered: true},
		RetryPolicy:  retry.DefaultPolicy,
		Suppressions: ss,
		Log:          zap.NewNop(),
	})
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

// ── POST /v1/notifications/suppression ───────────────────────────────────────

func TestAddSuppression_OK(t *testing.T) {
	ss := &stubSuppressionStore{}
	srv := newSuppressionHandler(t, ss)

	body := `{"recipient_email":"alice@example.com","reason":"HARD_BOUNCE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/notifications/suppression/", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-1")
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	require.Len(t, ss.suppressions, 1)
	assert.Equal(t, "alice@example.com", ss.suppressions[0].RecipientEmail)
	assert.Equal(t, ledger.SuppressionReasonHardBounce, ss.suppressions[0].Reason)
}

func TestAddSuppression_MissingFields(t *testing.T) {
	ss := &stubSuppressionStore{}
	srv := newSuppressionHandler(t, ss)

	body := `{"recipient_email":"alice@example.com"}` // missing reason
	req := httptest.NewRequest(http.MethodPost, "/v1/notifications/suppression/", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-1")
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Len(t, ss.suppressions, 0)
}

func TestAddSuppression_MissingPrincipal(t *testing.T) {
	ss := &stubSuppressionStore{}
	srv := newSuppressionHandler(t, ss)

	body := `{"recipient_email":"alice@example.com","reason":"COMPLAINT"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/notifications/suppression/", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	// No X-Principal-Id header
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, ss.suppressions)
}

// ── GET /v1/notifications/suppression ────────────────────────────────────────

func TestListSuppressions_OK(t *testing.T) {
	providerName := "sendgrid"
	ss := &stubSuppressionStore{suppressions: []*ledger.EmailSuppression{
		{
			SuppressionID:  "s-1",
			TenantID:       "tenant-abc",
			RecipientEmail: "bob@example.com",
			Reason:         ledger.SuppressionReasonComplaint,
			SourceStream:   "ALL",
			ProviderName:   &providerName,
			CreatedAt:      time.Now().UTC(),
		},
	}}
	srv := newSuppressionHandler(t, ss)

	req := httptest.NewRequest(http.MethodGet, "/v1/notifications/suppression/", nil)
	req.Header.Set("X-Principal-Id", "principal-1")
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var result []*ledger.EmailSuppression
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Len(t, result, 1)
	assert.Equal(t, "bob@example.com", result[0].RecipientEmail)
}

func TestListSuppressions_EmptyList(t *testing.T) {
	ss := &stubSuppressionStore{}
	srv := newSuppressionHandler(t, ss)

	req := httptest.NewRequest(http.MethodGet, "/v1/notifications/suppression/", nil)
	req.Header.Set("X-Principal-Id", "principal-1")
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var result []*ledger.EmailSuppression
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	// Must return [] not null (JSON null would confuse clients)
	assert.NotNil(t, result)
	assert.Empty(t, result)
}

// ── DELETE /v1/notifications/suppression/{email} ─────────────────────────────

func TestRemoveSuppression_OK(t *testing.T) {
	ss := &stubSuppressionStore{suppressions: []*ledger.EmailSuppression{
		{TenantID: "tenant-abc", RecipientEmail: "alice@example.com", Reason: ledger.SuppressionReasonHardBounce, SourceStream: "ALL"},
	}}
	srv := newSuppressionHandler(t, ss)

	email := url.QueryEscape("alice@example.com")
	req := httptest.NewRequest(http.MethodDelete, "/v1/notifications/suppression/"+email, nil)
	req.Header.Set("X-Principal-Id", "principal-1")
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, 1, ss.removeCalls)
}

// ── POST /v1/notifications/unsubscribe ───────────────────────────────────────

func TestHandleUnsubscribe_FormBody(t *testing.T) {
	ss := &stubSuppressionStore{}
	srv := newSuppressionHandler(t, ss)

	// Simulate a mail client's RFC 8058 one-click POST
	formData := url.Values{}
	formData.Set("List-Unsubscribe", "One-Click")
	formData.Set("action_token", "dummytoken")
	req := httptest.NewRequest(http.MethodPost,
		"/v1/notifications/unsubscribe?tenant_id=tenant-abc&email=alice%40example.com",
		bytes.NewBufferString(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// No auth headers — RFC 8058 request from mail client
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	// Must return 200 per RFC 8058 even if token is dummy
	assert.Equal(t, http.StatusOK, w.Code)
	// Suppression must be recorded
	require.Len(t, ss.suppressions, 1)
	assert.Equal(t, "alice@example.com", ss.suppressions[0].RecipientEmail)
	assert.Equal(t, ledger.SuppressionReasonUnsubscribe, ss.suppressions[0].Reason)
	assert.Equal(t, "tenant-abc", ss.suppressions[0].TenantID)
}

func TestHandleUnsubscribe_NoTenantOrEmail(t *testing.T) {
	ss := &stubSuppressionStore{}
	srv := newSuppressionHandler(t, ss)

	// Missing tenant_id and email — fail-closed returns 400 Bad Request
	req := httptest.NewRequest(http.MethodPost, "/v1/notifications/unsubscribe", nil)
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, ss.suppressions)
}

func TestHandleUnsubscribe_EmailNormalization(t *testing.T) {
	ss := &stubSuppressionStore{}
	srv := newSuppressionHandler(t, ss)

	formData := url.Values{}
	formData.Set("List-Unsubscribe", "One-Click")
	req := httptest.NewRequest(http.MethodPost,
		"/v1/notifications/unsubscribe?tenant_id=tenant-abc&email=Bob.Smith%2Bpromo%40Example.COM",
		bytes.NewBufferString(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.Len(t, ss.suppressions, 1)
	assert.Equal(t, "bob.smith+promo@example.com", ss.suppressions[0].RecipientEmail)
}

func TestHandleUnsubscribe_Idempotent(t *testing.T) {
	ss := &stubSuppressionStore{}
	srv := newSuppressionHandler(t, ss)

	sendUnsubscribe := func() {
		formData := url.Values{}
		formData.Set("List-Unsubscribe", "One-Click")
		req := httptest.NewRequest(http.MethodPost,
			"/v1/notifications/unsubscribe?tenant_id=tenant-abc&email=carol%40example.com",
			bytes.NewBufferString(formData.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}

	sendUnsubscribe()
	sendUnsubscribe()
	sendUnsubscribe()

	// Should be idempotent — AddSuppression uses ON CONFLICT DO UPDATE
	assert.Equal(t, 3, ss.addCalls) // called each time
	assert.Len(t, ss.suppressions, 1) // stored only once
}
