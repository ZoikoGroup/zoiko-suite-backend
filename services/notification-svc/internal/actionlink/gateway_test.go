package actionlink_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/actionlink"
	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/store"
)

type mockActionTokenStore struct {
	mu            sync.Mutex
	tokens        map[string]*ledger.ActionToken // key: tenantID + ":" + tokenHash
	consumedCount int
}

func newMockActionTokenStore() *mockActionTokenStore {
	return &mockActionTokenStore{
		tokens: make(map[string]*ledger.ActionToken),
	}
}

func (m *mockActionTokenStore) GetActionTokenByHash(ctx context.Context, tenantID, tokenHash string) (*ledger.ActionToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tenantID + ":" + tokenHash
	tok, ok := m.tokens[key]
	if !ok {
		return nil, store.ErrActionTokenNotFound
	}
	return tok, nil
}

func (m *mockActionTokenStore) ConsumeActionToken(ctx context.Context, tenantID, tokenHash, clientIP string) (*ledger.ActionToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tenantID + ":" + tokenHash
	tok, ok := m.tokens[key]
	if !ok {
		return nil, store.ErrActionTokenNotFound
	}
	if tok.Status == ledger.ActionTokenStatusConsumed {
		return nil, store.ErrActionTokenAlreadyConsumed
	}
	if tok.Status == ledger.ActionTokenStatusRevoked {
		return nil, store.ErrActionTokenRevoked
	}
	if tok.Status == ledger.ActionTokenStatusExpired || time.Now().UTC().After(tok.ExpiresAt) {
		tok.Status = ledger.ActionTokenStatusExpired
		return nil, store.ErrActionTokenExpired
	}

	m.consumedCount++
	now := time.Now().UTC()
	tok.Status = ledger.ActionTokenStatusConsumed
	tok.ConsumedAt = &now
	tok.ConsumedByIP = &clientIP
	return tok, nil
}

func setupGateway(t *testing.T) (*actionlink.Gateway, *actionlink.Signer, *mockActionTokenStore, chi.Router) {
	t.Helper()
	signer, err := actionlink.NewSigner(testKey, "https://notify.zoiko.com")
	require.NoError(t, err)

	mockStore := newMockActionTokenStore()
	gw, err := actionlink.NewGateway(mockStore, signer, zap.NewNop())
	require.NoError(t, err)

	r := chi.NewRouter()
	gw.RegisterRoutes(r)

	return gw, signer, mockStore, r
}

func TestGateway_LinkScannerGET_DoesNotMutateState(t *testing.T) {
	_, signer, mockStore, r := setupGateway(t)

	tenantID := "tenant-scanner-safe"
	token, signedTokenStr, err := signer.GenerateToken(
		tenantID, "intent-001", "usr-001", "VERIFY_EMAIL", "https://auth.zoiko.com/done", "POST",
		nil, 1*time.Hour,
	)
	require.NoError(t, err)

	mockStore.tokens[tenantID+":"+token.TokenHash] = token

	// Automated link scanner executes HTTP GET
	req := httptest.NewRequest(http.MethodGet, "/v1/notifications/actions/"+signedTokenStr, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// Invariants:
	// 1. HTTP 200 OK returned with read-only HTML landing page
	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "Confirm Action")
	assert.Contains(t, body, "Verify Email Address")
	assert.Contains(t, body, "confirm-action-button")

	// 2. CRITICAL: Store was NOT mutated! Token remains ACTIVE!
	assert.Equal(t, 0, mockStore.consumedCount, "link scanners must NEVER consume tokens")
	assert.Equal(t, ledger.ActionTokenStatusActive, token.Status)
	assert.Nil(t, token.ConsumedAt)
}

func TestGateway_LinkScannerGET_AcceptJSON(t *testing.T) {
	_, signer, mockStore, r := setupGateway(t)

	tenantID := "tenant-json"
	token, signedTokenStr, err := signer.GenerateToken(
		tenantID, "intent-002", "usr-002", "RESET_PASSWORD", "https://auth.zoiko.com/reset", "POST",
		nil, 1*time.Hour,
	)
	require.NoError(t, err)
	mockStore.tokens[tenantID+":"+token.TokenHash] = token

	req := httptest.NewRequest(http.MethodGet, "/v1/notifications/actions/"+signedTokenStr, nil)
	req.Header.Set("Accept", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	err = json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "ACTIVE", resp["status"])
	assert.Equal(t, "RESET_PASSWORD", resp["purpose"])
	assert.Equal(t, true, resp["requires_confirmation"])
	assert.Equal(t, 0, mockStore.consumedCount)
}

func TestGateway_ConfirmedPOST_ConsumesToken_AndRedirects(t *testing.T) {
	_, signer, mockStore, r := setupGateway(t)

	tenantID := "tenant-execute"
	targetURL := "https://auth.zoiko.com/email-verified"
	token, signedTokenStr, err := signer.GenerateToken(
		tenantID, "intent-003", "usr-003", "VERIFY_EMAIL", targetURL, "POST",
		nil, 1*time.Hour,
	)
	require.NoError(t, err)
	mockStore.tokens[tenantID+":"+token.TokenHash] = token

	// User clicks "Confirm & Complete Action" (HTTP POST to /execute)
	req := httptest.NewRequest(http.MethodPost, "/v1/notifications/actions/"+signedTokenStr+"/execute", nil)
	req.RemoteAddr = "198.51.100.22:45678"
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// Assert redirect to target destination URL
	assert.Equal(t, http.StatusSeeOther, rr.Code)
	assert.Equal(t, targetURL, rr.Header().Get("Location"))

	// Assert single-use consumption in store
	assert.Equal(t, 1, mockStore.consumedCount)
	assert.Equal(t, ledger.ActionTokenStatusConsumed, token.Status)
	assert.NotNil(t, token.ConsumedAt)
	assert.NotNil(t, token.ConsumedByIP)
	assert.Equal(t, "198.51.100.22", *token.ConsumedByIP)
}

func TestGateway_ConfirmedPOST_JSONResponse(t *testing.T) {
	_, signer, mockStore, r := setupGateway(t)

	tenantID := "tenant-json-post"
	targetURL := "https://auth.zoiko.com/email-verified"
	token, signedTokenStr, err := signer.GenerateToken(
		tenantID, "intent-004", "usr-004", "VERIFY_EMAIL", targetURL, "POST",
		nil, 1*time.Hour,
	)
	require.NoError(t, err)
	mockStore.tokens[tenantID+":"+token.TokenHash] = token

	req := httptest.NewRequest(http.MethodPost, "/v1/notifications/actions/"+signedTokenStr+"/execute", nil)
	req.Header.Set("Accept", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	err = json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "CONSUMED", resp["status"])
	assert.Equal(t, targetURL, resp["target_action_url"])
}

func TestGateway_ReplayPOST_FailsWithConflict(t *testing.T) {
	_, signer, mockStore, r := setupGateway(t)

	tenantID := "tenant-replay"
	token, signedTokenStr, err := signer.GenerateToken(
		tenantID, "intent-005", "usr-005", "RESET_PASSWORD", "https://example.com", "POST",
		nil, 1*time.Hour,
	)
	require.NoError(t, err)
	mockStore.tokens[tenantID+":"+token.TokenHash] = token

	// First execution -> SUCCESS
	req1 := httptest.NewRequest(http.MethodPost, "/v1/notifications/actions/"+signedTokenStr+"/execute", nil)
	rr1 := httptest.NewRecorder()
	r.ServeHTTP(rr1, req1)
	assert.Equal(t, http.StatusSeeOther, rr1.Code)

	// Second execution (replay) -> FAILS (409 Conflict)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/notifications/actions/"+signedTokenStr+"/execute", nil)
	rr2 := httptest.NewRecorder()
	r.ServeHTTP(rr2, req2)
	assert.Equal(t, http.StatusConflict, rr2.Code)

	// GET after consumption -> FAILS (410 Gone)
	req3 := httptest.NewRequest(http.MethodGet, "/v1/notifications/actions/"+signedTokenStr, nil)
	rr3 := httptest.NewRecorder()
	r.ServeHTTP(rr3, req3)
	assert.Equal(t, http.StatusGone, rr3.Code)
}

func TestGateway_ExpiredToken_Rejected(t *testing.T) {
	_, signer, mockStore, r := setupGateway(t)

	tenantID := "tenant-exp"
	token, signedTokenStr, err := signer.GenerateToken(
		tenantID, "intent-006", "usr-006", "WORKSPACE_INVITE", "https://example.com", "POST",
		nil, 1*time.Hour,
	)
	require.NoError(t, err)
	token.Status = ledger.ActionTokenStatusExpired
	token.ExpiresAt = time.Now().UTC().Add(-10 * time.Minute)
	mockStore.tokens[tenantID+":"+token.TokenHash] = token

	// GET expired -> 410 Gone
	reqGet := httptest.NewRequest(http.MethodGet, "/v1/notifications/actions/"+signedTokenStr, nil)
	rrGet := httptest.NewRecorder()
	r.ServeHTTP(rrGet, reqGet)
	assert.Equal(t, http.StatusGone, rrGet.Code)

	// POST expired -> 410 Gone
	reqPost := httptest.NewRequest(http.MethodPost, "/v1/notifications/actions/"+signedTokenStr+"/execute", nil)
	rrPost := httptest.NewRecorder()
	r.ServeHTTP(rrPost, reqPost)
	assert.Equal(t, http.StatusGone, rrPost.Code)
}

func TestGateway_TamperedSignature_Rejected(t *testing.T) {
	_, signer, _, r := setupGateway(t)

	_, signedTokenStr, err := signer.GenerateToken(
		"tenant-tamper", "intent-007", "usr-007", "VERIFY_EMAIL", "https://example.com", "POST",
		nil, 1*time.Hour,
	)
	require.NoError(t, err)

	// Tamper token string
	tampered := signedTokenStr + "tampered"

	reqGet := httptest.NewRequest(http.MethodGet, "/v1/notifications/actions/"+tampered, nil)
	rrGet := httptest.NewRecorder()
	r.ServeHTTP(rrGet, reqGet)
	assert.Equal(t, http.StatusBadRequest, rrGet.Code)

	reqPost := httptest.NewRequest(http.MethodPost, "/v1/notifications/actions/"+tampered+"/execute", nil)
	rrPost := httptest.NewRecorder()
	r.ServeHTTP(rrPost, reqPost)
	assert.Equal(t, http.StatusBadRequest, rrPost.Code)
}

func TestGateway_TokenNotFound_Returns404(t *testing.T) {
	_, signer, _, r := setupGateway(t)

	// Valid signature for a token that does not exist in store
	_, signedTokenStr, err := signer.GenerateToken(
		"tenant-unknown", "intent-008", "usr-008", "VERIFY_EMAIL", "https://example.com", "POST",
		nil, 1*time.Hour,
	)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/notifications/actions/"+signedTokenStr, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}
