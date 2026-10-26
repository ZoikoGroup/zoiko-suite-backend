package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/clients"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

type fakeBNK02Store struct {
	initiateParams  domain.InitiateConnectionParams
	initiateCalled  bool
	completeCalled  bool
	activateCalled  bool
	suspendCalled   bool
	reconnectCalled bool
	conn            *domain.BankConnection
}

func (f *fakeBNK02Store) InitiateConnection(_ context.Context, params domain.InitiateConnectionParams) (*domain.BankConnection, bool, error) {
	f.initiateParams = params
	f.initiateCalled = true
	return &domain.BankConnection{
		ConnectionID:  "conn-from-store",
		TenantID:      params.TenantID,
		LegalEntityID: params.LegalEntityID,
		BankAccountID: params.BankAccountID,
		BankName:      params.BankName,
		Currency:      params.Currency,
		Status:        domain.ConnStatusRequested,
	}, true, nil
}

func (f *fakeBNK02Store) CompleteConnectionAuthorization(_ context.Context, _ domain.CompleteConnectionAuthorizationParams) (*domain.BankConnection, error) {
	f.completeCalled = true
	return f.conn, nil
}

func (f *fakeBNK02Store) ActivateConnection(_ context.Context, _ domain.ActivateConnectionParams) (*domain.BankConnection, error) {
	f.activateCalled = true
	return f.conn, nil
}

func (f *fakeBNK02Store) RefreshConnection(_ context.Context, _ domain.RefreshConnectionParams) (*domain.BankConnection, error) {
	return f.conn, nil
}

func (f *fakeBNK02Store) TriggerReconsent(_ context.Context, _ domain.TriggerReconsentParams) (*domain.BankConnection, error) {
	return f.conn, nil
}

func (f *fakeBNK02Store) SuspendConnection(_ context.Context, _ domain.SuspendConnectionParams) (*domain.BankConnection, error) {
	f.suspendCalled = true
	c := *f.conn
	c.Status = domain.ConnStatusSuspended
	return &c, nil
}

func (f *fakeBNK02Store) RevokeConnection(_ context.Context, _ domain.RevokeConnectionParams) (*domain.BankConnection, error) {
	return f.conn, nil
}

func (f *fakeBNK02Store) ReconnectProvider(_ context.Context, _ domain.ReconnectProviderParams) (*domain.BankConnection, error) {
	f.reconnectCalled = true
	return f.conn, nil
}

func (f *fakeBNK02Store) RotateConnectionCredential(_ context.Context, _ domain.RotateConnectionCredentialParams) (*domain.BankConnection, error) {
	return f.conn, nil
}

func (f *fakeBNK02Store) ListConnectionEvents(_ context.Context, _, _ string) ([]domain.ConnectionEvent, error) {
	return nil, nil
}

func (f *fakeBNK02Store) CreateRegionPolicy(_ context.Context, _ domain.CreateRegionPolicyParams) (*domain.BankRegionPolicy, error) {
	return nil, nil
}

func (f *fakeBNK02Store) IsRegionAllowed(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}

func (f *fakeBNK02Store) HasRegionPolicy(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

type fakeBankAccounts struct {
	ref   *clients.BankAccountRef
	err   error
	calls int
}

func (f *fakeBankAccounts) VerifyBankAccount(_ context.Context, _, _, _, _ string) (*clients.BankAccountRef, error) {
	f.calls++
	return f.ref, f.err
}

type fakeVault struct{ calls int }

func (f *fakeVault) RevokeLease(_ context.Context, _, _, _, _ string) error {
	f.calls++
	return nil
}

func setupBNK02TestRouter(t *testing.T, bnk store.BNK02Store, accounts clients.BankAccountClient, vault clients.VaultClient) (chi.Router, *store.MemoryStore) {
	t.Helper()
	mem := store.NewMemoryStore()
	az := authz.NewClient(newGrantingAuthzServer(t).URL)
	h := New(mem, az, zap.NewNop())

	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterBNK02Routes(r, h, bnk, accounts, vault)
	return r, mem
}

func doBNK02Request(t *testing.T, r chi.Router, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-bnk02-handler")
	req.Header.Set("X-Principal-Id", "ops-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func seedHandlerConnection(t *testing.T, mem *store.MemoryStore, conn *domain.BankConnection) {
	t.Helper()
	ctx := middleware.WithTenant(context.Background(), "tenant-bnk02-handler")
	if err := mem.CreateConnection(ctx, conn, "ops-1", "corr-seed"); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
}

func TestBNK02_InitiateConnection_BindsVerifiedLegalEntityAndCurrency(t *testing.T) {
	bnk := &fakeBNK02Store{}
	accounts := &fakeBankAccounts{ref: &clients.BankAccountRef{
		BankAccountID: "acct-eur", TenantID: "tenant-bnk02-handler", LegalEntityID: "le-eur", CurrencyCode: "EUR",
	}}
	r, _ := setupBNK02TestRouter(t, bnk, accounts, &fakeVault{})

	body := map[string]any{
		"legal_entity_id": "le-eur", "bank_account_id": "acct-eur", "bank_name": "Euro Bank", "correlation_id": "corr-eur",
	}
	rec := doBNK02Request(t, r, http.MethodPost, "/v1/banking/connections/request", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bnk.initiateCalled {
		t.Fatal("expected InitiateConnection to be called")
	}
	if bnk.initiateParams.Currency != "EUR" {
		t.Fatalf("expected account currency EUR, got %q", bnk.initiateParams.Currency)
	}
}

func TestBNK02_InitiateConnection_RejectsAccountFromAnotherLegalEntity(t *testing.T) {
	bnk := &fakeBNK02Store{}
	accounts := &fakeBankAccounts{ref: &clients.BankAccountRef{
		BankAccountID: "acct-other", TenantID: "tenant-bnk02-handler", LegalEntityID: "le-b", CurrencyCode: "USD",
	}}
	r, _ := setupBNK02TestRouter(t, bnk, accounts, &fakeVault{})

	body := map[string]any{
		"legal_entity_id": "le-a", "bank_account_id": "acct-other", "bank_name": "Bank", "correlation_id": "corr-cross-entity",
	}
	rec := doBNK02Request(t, r, http.MethodPost, "/v1/banking/connections/request", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for cross-entity account, got %d: %s", rec.Code, rec.Body.String())
	}
	if bnk.initiateCalled {
		t.Fatal("a cross-entity account must never reach InitiateConnection")
	}
}

func TestBNK02_CompleteAuthorization_RejectsScopeExpansion(t *testing.T) {
	bnk := &fakeBNK02Store{}
	r, mem := setupBNK02TestRouter(t, bnk, &fakeBankAccounts{}, &fakeVault{})
	conn := &domain.BankConnection{
		LegalEntityID: "le-scope", Status: domain.ConnStatusRequested, ConsentScope: []string{"balances"},
	}
	seedHandlerConnection(t, mem, conn)

	body := map[string]any{
		"token_lease_ref": "lease-1", "token_expires_at": time.Now().Add(time.Hour), "granted_scope": []string{"balances", "payments"},
	}
	rec := doBNK02Request(t, r, http.MethodPost, "/v1/banking/connections/"+conn.ConnectionID+"/complete-authorization", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for scope expansion, got %d: %s", rec.Code, rec.Body.String())
	}
	if bnk.completeCalled {
		t.Fatal("scope expansion must be refused before the store write")
	}
}

func TestBNK02_CompleteAuthorization_RejectsExpiredToken(t *testing.T) {
	bnk := &fakeBNK02Store{}
	r, mem := setupBNK02TestRouter(t, bnk, &fakeBankAccounts{}, &fakeVault{})
	conn := &domain.BankConnection{
		LegalEntityID: "le-expired", Status: domain.ConnStatusRequested, ConsentScope: []string{"balances"},
	}
	seedHandlerConnection(t, mem, conn)

	body := map[string]any{
		"token_lease_ref": "lease-1", "token_expires_at": time.Now().Add(-time.Minute), "granted_scope": []string{"balances"},
	}
	rec := doBNK02Request(t, r, http.MethodPost, "/v1/banking/connections/"+conn.ConnectionID+"/complete-authorization", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an expired token, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBNK02_ActivateConnection_RejectsExpiredCredential(t *testing.T) {
	bnk := &fakeBNK02Store{}
	r, mem := setupBNK02TestRouter(t, bnk, &fakeBankAccounts{}, &fakeVault{})
	expired := time.Now().Add(-time.Minute)
	conn := &domain.BankConnection{
		LegalEntityID: "le-activate", Status: domain.ConnStatusAuthorizing, TokenLeaseRef: "lease-old", TokenExpiresAt: &expired,
	}
	seedHandlerConnection(t, mem, conn)

	rec := doBNK02Request(t, r, http.MethodPost, "/v1/banking/connections/"+conn.ConnectionID+"/activate", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 activating an expired credential, got %d: %s", rec.Code, rec.Body.String())
	}
	if bnk.activateCalled {
		t.Fatal("an expired credential must never reach ActivateConnection")
	}
}

func TestBNK02_SuspendConnection_DoesNotRevokeReversibleCredential(t *testing.T) {
	bnk := &fakeBNK02Store{}
	vault := &fakeVault{}
	r, mem := setupBNK02TestRouter(t, bnk, &fakeBankAccounts{}, vault)
	expires := time.Now().Add(time.Hour)
	conn := &domain.BankConnection{
		LegalEntityID: "le-suspend", Status: domain.ConnStatusActive, TokenLeaseRef: "lease-live", TokenExpiresAt: &expires,
	}
	seedHandlerConnection(t, mem, conn)
	bnk.conn = conn

	rec := doBNK02Request(t, r, http.MethodPost, "/v1/banking/connections/"+conn.ConnectionID+"/suspend", map[string]string{"reason": "review"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if vault.calls != 0 {
		t.Fatalf("suspend must not revoke the lease; ReconnectProvider needs it. vault calls=%d", vault.calls)
	}
}

func TestBNK02_ReconnectConnection_RejectsExpiredCredential(t *testing.T) {
	bnk := &fakeBNK02Store{}
	r, mem := setupBNK02TestRouter(t, bnk, &fakeBankAccounts{}, &fakeVault{})
	expired := time.Now().Add(-time.Minute)
	conn := &domain.BankConnection{
		LegalEntityID: "le-reconnect", Status: domain.ConnStatusSuspended, TokenLeaseRef: "lease-old", TokenExpiresAt: &expired,
	}
	seedHandlerConnection(t, mem, conn)

	rec := doBNK02Request(t, r, http.MethodPost, "/v1/banking/connections/"+conn.ConnectionID+"/reconnect", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 reconnecting an expired credential, got %d: %s", rec.Code, rec.Body.String())
	}
	if bnk.reconnectCalled {
		t.Fatal("an expired credential must never reach ReconnectProvider")
	}
}
