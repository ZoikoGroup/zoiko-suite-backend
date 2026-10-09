package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	"zoiko.io/notification-svc/internal/unsubscribe"
)

// ── stubSuppressionStore ──────────────────────────────────────────────────────

type stubSuppressionStore struct {
	suppressions []*ledger.EmailSuppression
	addCalls     int
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

// testUnsubscribe is the codec the handler under test opens tokens with.
var testUnsubscribe = func() *unsubscribe.Codec {
	c, err := unsubscribe.New([]byte("test-unsubscribe-secret-0123456789abcdef"), "https://notify.example.test")
	if err != nil {
		panic(err)
	}
	return c
}()

func newSuppressionHandler(t *testing.T, ss *stubSuppressionStore) http.Handler {
	t.Helper()
	h := handler.New(handler.Deps{
		Unsubscribe:  testUnsubscribe,
		Store:        &stubStore{byID: make(map[string]*domain.Notification), templates: make(map[string]*domain.TemplateDefinition), versions: make(map[string]*domain.TemplateVersion)},
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

// There is no DELETE: a suppression is lifted on evidence through the governed
// NCD lift and never removed (§7.3, migration 000022).
func TestRemoveSuppression_RouteIsGone(t *testing.T) {
	ss := &stubSuppressionStore{suppressions: []*ledger.EmailSuppression{
		{TenantID: "tenant-abc", RecipientEmail: "alice@example.com", Reason: ledger.SuppressionReasonHardBounce, SourceStream: "ALL"},
	}}
	srv := newSuppressionHandler(t, ss)

	req := httptest.NewRequest(http.MethodDelete, "/v1/notifications/suppression/"+url.QueryEscape("alice@example.com"), nil)
	req.Header.Set("X-Principal-Id", "principal-1")
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	assert.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, w.Code)
	assert.Len(t, ss.suppressions, 1, "the suppression must survive")
}

// ── POST /v1/notifications/unsubscribe ───────────────────────────────────────

func unsubscribeURL(t *testing.T, tenantID, email string) string {
	t.Helper()
	h, err := testUnsubscribe.Headers(tenantID, email)
	require.NoError(t, err)
	u := strings.TrimSuffix(strings.TrimPrefix(h["List-Unsubscribe"], "<https://notify.example.test"), ">")
	return u
}

// oneClick posts the RFC 8058 body to target, as a mail client does.
func oneClick(srv http.Handler, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader("List-Unsubscribe=One-Click"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestHandleUnsubscribe_ValidTokenRecordsUnsubscribe(t *testing.T) {
	ss := &stubSuppressionStore{}
	srv := newSuppressionHandler(t, ss)

	w := oneClick(srv, unsubscribeURL(t, "tenant-abc", "Bob.Smith+promo@Example.COM"))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, ss.suppressions, 1)
	assert.Equal(t, "tenant-abc", ss.suppressions[0].TenantID)
	assert.Equal(t, "bob.smith+promo@example.com", ss.suppressions[0].RecipientEmail)
	assert.Equal(t, ledger.SuppressionReasonUnsubscribe, ss.suppressions[0].Reason)
}

// The forgery that used to work: tenant and address named in the request,
// with no token or a made-up one. Nothing is written.
func TestHandleUnsubscribe_ForgedRequestsWriteNothing(t *testing.T) {
	ss := &stubSuppressionStore{}
	srv := newSuppressionHandler(t, ss)

	valid := unsubscribeURL(t, "tenant-abc", "alice@example.com")
	tampered := valid[:len(valid)-2] + "AA"
	cases := map[string]struct {
		target string
		want   int
	}{
		"bare tenant and email":  {"/v1/notifications/unsubscribe?tenant_id=tenant-abc&email=victim%40example.com", http.StatusBadRequest},
		"dummy token":            {"/v1/notifications/unsubscribe?token=dummytoken&tenant_id=tenant-abc&email=victim%40example.com", http.StatusForbidden},
		"tampered token":         {tampered, http.StatusForbidden},
		"old action-token shape": {"/v1/notifications/unsubscribe?action_token=dGVuYW50LWFiYy5VTlNVQi54Lnk", http.StatusBadRequest}, // gitleaks:allow — base64 of the deliberately-rejected legacy action-token shape (tenant-abc.UNSUB...), a test fixture, not a live credential
	}
	for name, c := range cases {
		w := oneClick(srv, c.target)
		assert.Equal(t, c.want, w.Code, name)
	}
	assert.Empty(t, ss.suppressions, "a refused unsubscribe must write nothing")
}

// A valid token for one address cannot be redirected at another by adding
// parameters: only the token is believed.
func TestHandleUnsubscribe_ParametersCannotOverrideToken(t *testing.T) {
	ss := &stubSuppressionStore{}
	srv := newSuppressionHandler(t, ss)

	w := oneClick(srv, unsubscribeURL(t, "tenant-abc", "alice@example.com")+"&tenant_id=tenant-evil&email=victim%40example.com")

	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, ss.suppressions, 1)
	assert.Equal(t, "tenant-abc", ss.suppressions[0].TenantID)
	assert.Equal(t, "alice@example.com", ss.suppressions[0].RecipientEmail)
}

func TestHandleUnsubscribe_NotConfiguredRefusesEverything(t *testing.T) {
	ss := &stubSuppressionStore{}
	h := handler.New(handler.Deps{
		Store:        &stubStore{byID: make(map[string]*domain.Notification), templates: make(map[string]*domain.TemplateDefinition), versions: make(map[string]*domain.TemplateVersion)},
		AuthZ:        &stubAuthZ{},
		Deliverer:    &stubDeliverer{delivered: true},
		RetryPolicy:  retry.DefaultPolicy,
		Suppressions: ss,
		Log:          zap.NewNop(),
	})
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	w := oneClick(r, unsubscribeURL(t, "tenant-abc", "alice@example.com"))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Empty(t, ss.suppressions)
}

func TestHandleUnsubscribe_Idempotent(t *testing.T) {
	ss := &stubSuppressionStore{}
	srv := newSuppressionHandler(t, ss)
	target := unsubscribeURL(t, "tenant-abc", "carol@example.com")

	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusOK, oneClick(srv, target).Code)
	}
	assert.Equal(t, 3, ss.addCalls)
	assert.Len(t, ss.suppressions, 1)
}
