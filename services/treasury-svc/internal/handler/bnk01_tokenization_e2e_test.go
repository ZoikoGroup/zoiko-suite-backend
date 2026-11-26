//go:build integration

// Real end-to-end proof, through this service's real HTTP handler and
// router, a real PgStore against a real Postgres, and the real HTTP
// tokenization.Client speaking key-management-svc's actual /v1/tokenize and
// /v1/detokenize wire contract (envelope headers required, tenant-bound
// tokens, 403 for a principal without TOKEN_DETOKENIZE) — against
// vaultServer, which answers exactly that contract. This is the same
// precedent banking-connector-svc's and payee-banking-identity-svc's own
// e2e_tokenized_integration_test.go already established for this exact
// problem: a fake HTTP server standing in for key-management-svc's wire
// contract (not its internals, which are proven by key-management-svc's own
// tests), plus a Go-level authz stub standing in for authorization-svc's
// grant decision (its own RBAC/SoD/ABAC engine is proven by
// authorization-svc's own tests) — so this test proves THIS service's own
// contribution: real masking/tokenization wired into RegisterBankAccount,
// RotateAccountIdentifierToken and the new privileged account-number route,
// without re-proving services this change does not touch.
package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	"zoiko.io/treasury-svc/internal/handler"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
	"zoiko.io/treasury-svc/internal/store"
	"zoiko.io/treasury-svc/internal/tokenization"
)

const rawAccountNumber = "GB29NWBK60161331926819" // ends 6819, same fixture digits the sibling services' own e2e tests use

// e2eDatabase creates a fresh database (via a short-lived embedded Postgres
// unless TEST_DATABASE_URL is set, matching internal/store's own TestMain
// convention) and applies every migration to it.
func e2eDatabase(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	var pg *embeddedpostgres.EmbeddedPostgres
	if dsn == "" {
		dbPort := uint32(15901 + uint32(os.Getpid()%399))
		pg = embeddedpostgres.NewDatabase(
			embeddedpostgres.DefaultConfig().
				Version(embeddedpostgres.PostgresVersion("16.15.0")).
				Port(dbPort).
				Database("treasury_handler_e2e").
				Username("postgres").
				Password("postgres").
				RuntimePath(filepath.Join(os.TempDir(), fmt.Sprintf("epg-treasury-handler-%d", dbPort))),
		)
		if err := pg.Start(); err != nil {
			t.Fatalf("failed to start embedded postgres: %v", err)
		}
		dsn = fmt.Sprintf("host=localhost port=%d dbname=treasury_handler_e2e user=postgres password=postgres sslmode=disable", dbPort)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		if pg != nil {
			_ = pg.Stop()
		}
		t.Fatalf("connect: %v", err)
	}
	for i := 0; i < 75; i++ {
		if err = pool.Ping(ctx); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		pool.Close()
		if pg != nil {
			_ = pg.Stop()
		}
		t.Fatalf("postgres did not become ready: %v", err)
	}
	_, here, _, _ := runtime.Caller(0)
	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_idempotency_index.up.sql",
		"000003_add_bnk01_identity.up.sql",
		"000004_add_bnk09_treasury_transfer.up.sql",
		"000005_add_bnk10_fx_rates.up.sql",
		"000006_add_bnk01_account_history.up.sql",
		"000007_add_bnk09_lifecycle_extensions.up.sql",
		"000008_add_bnk08_cash_position_snapshot.up.sql",
		"000009_add_bnk10_fx_exposure_snapshot.up.sql",
		"000010_add_bnk09_authorization_and_events.up.sql",
		"000011_add_eventing_outbox.up.sql",
		"000012_eventing_outbox_rls.up.sql",
		"000013_bnk01_history_reason_columns.up.sql",
		"000014_eventing_inbox.up.sql",
		"000015_add_bnk01_identifier_tokens.up.sql",
	} {
		sqlBytes, err := os.ReadFile(filepath.Join(filepath.Dir(here), "../../deployments/migrations", migration))
		if err != nil {
			t.Fatalf("read migration %s: %v", migration, err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply migration %s: %v", migration, err)
		}
	}
	cleanup := func() {
		pool.Close()
		if pg != nil {
			_ = pg.Stop()
		}
	}
	return pool, cleanup
}

// vaultServer speaks key-management-svc's real contract over HTTP, backed by
// a tokenization.Fake. detokenizers is the set of principals holding
// TOKEN_DETOKENIZE — everyone else gets a real 403, same as the live vault.
func vaultServer(t *testing.T, detokenizers map[string]bool) *httptest.Server {
	t.Helper()
	fake := &tokenization.Fake{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"X-Tenant-Id", "X-Principal-Id", "X-Legal-Entity-Id", "X-Request-Id", "X-Source-Channel", "X-Purpose-Context", "Idempotency-Key"} {
			if r.Header.Get(h) == "" {
				http.Error(w, "envelope_incomplete: "+h, http.StatusBadRequest)
				return
			}
		}
		scope := tokenization.Scope{TenantID: r.Header.Get("X-Tenant-Id"), PrincipalID: r.Header.Get("X-Principal-Id"), LegalEntityID: r.Header.Get("X-Legal-Entity-Id")}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/tokenize":
			var in struct{ Value, Classification string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			tok, err := fake.Tokenize(r.Context(), scope, in.Classification, in.Value)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": tok})
		case "/v1/detokenize":
			if !detokenizers[scope.PrincipalID] {
				http.Error(w, "not authorized", http.StatusForbidden)
				return
			}
			var in struct{ Token string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			v, err := fake.Detokenize(r.Context(), scope, in.Token)
			if err != nil {
				http.Error(w, "token is not available to this caller", http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"value": v})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// allowAllAuthz grants every action to every principal — this test proves
// BNK-01's own tokenization wiring, not authorization-svc's RBAC/SoD engine
// (proven separately, by that service's own tests).
type allowAllAuthz struct{}

func (allowAllAuthz) CheckAllowed(context.Context, string, string, string) error { return nil }

func doReq(r http.Handler, method, path string, body any, tenant, principal string) *httptest.ResponseRecorder {
	var buf strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = *strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", principal)
	req.Header.Set("X-Correlation-ID", "corr-"+uuid.NewString())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func newRouter(st *store.PgStore, tokenizer tokenization.Tokenizer, enforced bool) http.Handler {
	h := handler.New(st, allowAllAuthz{}, nil, nil, nil, zap.NewNop(), nil).WithTokenization(tokenizer, enforced)
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

// TestE2E_TokenizationEnforced_RealAccountNumberNeverStored proves the full
// chain: a real account number tokenized on create through the real vault
// wire contract, the DB holding only the token (never the raw value), an
// ordinary masked read serving the correct last4 with ZERO vault calls, the
// privileged account-number route correctly detokenizing for an authorized
// principal and refusing an unauthorized one, and rotation tokenizing a NEW
// real value.
func TestE2E_TokenizationEnforced_RealAccountNumberNeverStored(t *testing.T) {
	pool, cleanup := e2eDatabase(t)
	defer cleanup()
	vault := vaultServer(t, map[string]bool{"bank-detail-reader": true})
	tokenizer := tokenization.NewClient(vault.URL)
	st := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	r := newRouter(st, tokenizer, true)

	tenant, entity := uuid.NewString(), "le-e2e"

	// 1. Create, enforcement ON: the real account number is tokenized.
	w := doReq(r, http.MethodPost, "/v1/treasury/accounts", domain.RegisterBankAccountRequest{
		LegalEntityID: entity, AccountName: "E2E Checking", AccountNumber: rawAccountNumber,
		BankIdentifier: "NWBKGB2L", CurrencyCode: "GBP",
	}, tenant, "registrar")
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var created domain.BankAccount
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantMasked := domain.MaskAccountNumber(rawAccountNumber)
	if created.MaskedAccountNumber != wantMasked {
		t.Fatalf("expected server-computed last4 mask %q, got %q", wantMasked, created.MaskedAccountNumber)
	}
	if strings.Contains(w.Body.String(), rawAccountNumber) {
		t.Fatalf("raw account number leaked in create response: %s", w.Body.String())
	}

	// 2. The database holds a TOKEN, never the raw value, and vaultServer
	// really was called (Calls()>0 is implicit — a non-matching tenant/value
	// would have failed the create above).
	var storedMasked, token string
	if err := pool.QueryRow(context.Background(), `
		SELECT a.masked_account_number, t.account_number_token
		FROM bank_accounts a JOIN bank_account_identifier_tokens t USING (bank_account_id)
		WHERE a.bank_account_id = $1`, created.BankAccountID).Scan(&storedMasked, &token); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if storedMasked == rawAccountNumber || token == rawAccountNumber || token == "" {
		t.Fatalf("expected a real token distinct from the raw value, got masked=%q token=%q", storedMasked, token)
	}

	// 3. Ordinary masked read: correct last4, and it must not need the vault
	// at all — prove it by pointing the SAME handler at a vault URL that
	// refuses every request, and confirming the masked read still succeeds.
	deadVaultHandler := newRouter(st, tokenization.NewClient("http://127.0.0.1:1"), true)
	w = doReq(deadVaultHandler, http.MethodGet, "/v1/treasury/accounts/"+created.BankAccountID+"/masked", nil, tenant, "reader")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), wantMasked) {
		t.Fatalf("masked read (vault unreachable): %d %s", w.Code, w.Body.String())
	}

	// 4. Privileged route, authorized principal: the REAL account number.
	w = doReq(r, http.MethodGet, "/v1/treasury/accounts/"+created.BankAccountID+"/account-number", nil, tenant, "bank-detail-reader")
	if w.Code != http.StatusOK {
		t.Fatalf("privileged read (authorized): %d %s", w.Code, w.Body.String())
	}
	var revealed struct {
		AccountNumber string `json:"account_number"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &revealed)
	if revealed.AccountNumber != rawAccountNumber {
		t.Fatalf("expected the real account number back, got %q", revealed.AccountNumber)
	}

	// 5. Privileged route, UNAUTHORIZED principal (not in vaultServer's
	// detokenizers set): denied, never the number or the token.
	w = doReq(r, http.MethodGet, "/v1/treasury/accounts/"+created.BankAccountID+"/account-number", nil, tenant, "someone-else")
	if w.Code != http.StatusForbidden {
		t.Fatalf("privileged read (unauthorized): expected 403, got %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), rawAccountNumber) || strings.Contains(w.Body.String(), token) {
		t.Fatalf("denied response leaked the number or token: %s", w.Body.String())
	}

	// 6. Rotation: a NEW real value is tokenized, the OLD token is
	// superseded (no longer the current one), and the privileged read now
	// returns the NEW number.
	const newRaw = "DE89370400440532013000"
	w = doReq(r, http.MethodPost, "/v1/treasury/accounts/"+created.BankAccountID+"/rotate-token",
		map[string]string{"new_account_number": newRaw, "new_bank_identifier": "DEUTDEFF"}, tenant, "registrar")
	if w.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	var rotated domain.BankAccount
	_ = json.Unmarshal(w.Body.Bytes(), &rotated)
	wantRotatedMasked := domain.MaskAccountNumber(newRaw)
	if rotated.MaskedAccountNumber != wantRotatedMasked {
		t.Fatalf("expected the rotated mask %q, got %q", wantRotatedMasked, rotated.MaskedAccountNumber)
	}
	w = doReq(r, http.MethodGet, "/v1/treasury/accounts/"+created.BankAccountID+"/account-number", nil, tenant, "bank-detail-reader")
	_ = json.Unmarshal(w.Body.Bytes(), &revealed)
	if w.Code != http.StatusOK || revealed.AccountNumber != newRaw {
		t.Fatalf("post-rotation privileged read: %d %q, want %q", w.Code, revealed.AccountNumber, newRaw)
	}
}

// TestE2E_TokenizationNotEnforced_PreservesExactOldBehavior is the
// regression guard: with TOKENIZATION_ENFORCED=false (the default), the
// caller-supplied MaskedAccountNumber is stored exactly as before, no vault
// call ever happens (the tokenizer here points at an address nothing
// listens on), and the privileged account-number route correctly reports
// "not tokenized" rather than fabricating a value.
func TestE2E_TokenizationNotEnforced_PreservesExactOldBehavior(t *testing.T) {
	pool, cleanup := e2eDatabase(t)
	defer cleanup()
	st := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	deadTokenizer := tokenization.NewClient("http://127.0.0.1:1") // nothing listens here
	r := newRouter(st, deadTokenizer, false)

	tenant, entity := uuid.NewString(), "le-e2e-legacy"
	w := doReq(r, http.MethodPost, "/v1/treasury/accounts", domain.RegisterBankAccountRequest{
		LegalEntityID: entity, AccountName: "Legacy Checking", MaskedAccountNumber: "****LEGACY",
		BankIdentifier: "SWIFT-TEST", CurrencyCode: "USD",
	}, tenant, "registrar")
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var created domain.BankAccount
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.MaskedAccountNumber != "****LEGACY" {
		t.Fatalf("expected the exact caller-supplied masked value, got %q", created.MaskedAccountNumber)
	}

	var tokenRows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM bank_account_identifier_tokens WHERE bank_account_id = $1`,
		created.BankAccountID).Scan(&tokenRows); err != nil {
		t.Fatalf("count token rows: %v", err)
	}
	if tokenRows != 0 {
		t.Fatalf("expected zero token rows while enforcement is off, got %d", tokenRows)
	}

	// The privileged route refuses with 409, not a fabricated value — this
	// account never had a real value server-side to reveal.
	w = doReq(r, http.MethodGet, "/v1/treasury/accounts/"+created.BankAccountID+"/account-number", nil, tenant, "bank-detail-reader")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 not_tokenized for a non-tokenized account, got %d %s", w.Code, w.Body.String())
	}
}
