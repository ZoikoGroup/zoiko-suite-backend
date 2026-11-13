//go:build integration

package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payee-banking-identity-svc/internal/authz"
	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/handler"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
	"zoiko.io/payee-banking-identity-svc/internal/store"
	"zoiko.io/payee-banking-identity-svc/internal/tokenization"
)

// End to end through this service, for the read path payments depend on:
// the real handler and router, the real PgStore with tokenization enforced,
// and the real HTTP tokenization client — talking to vaultServer, which
// speaks key-management-svc's /v1/tokenize and /v1/detokenize contract
// (envelope headers required, tenant-bound tokens, 403 for a principal
// without TOKEN_DETOKENIZE). The live run against the real key-management-svc
// is in SPEC_DEVIATIONS.md B12; this is the same path, repeatable.

// e2eDatabase creates (or recreates) a database of this test's own on the
// TEST_DATABASE_URL server and applies every migration to it.
func e2eDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	root, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, stmt := range []string{`DROP DATABASE IF EXISTS payee_handler_e2e WITH (FORCE)`, `CREATE DATABASE payee_handler_e2e`} {
		if _, err := root.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	u, _ := url.Parse(dsn)
	u.Path = "/payee_handler_e2e"
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, here, _, _ := runtime.Caller(0)
	migrations, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "../../deployments/migrations/*.up.sql"))
	if len(migrations) == 0 {
		t.Fatal("no migrations")
	}
	for _, m := range migrations {
		sqlText, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sqlText)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(m), err)
		}
	}
	return pool
}

// vaultServer speaks key-management-svc's contract over HTTP, backed by the
// Fake's tenant-bound tokens. detokenizers is the set of principals holding
// TOKEN_DETOKENIZE; everyone else gets 403, as from the real vault.
func vaultServer(t *testing.T, detokenizers map[string]bool) *httptest.Server {
	fake := &tokenization.Fake{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"X-Tenant-Id", "X-Principal-Id", "X-Legal-Entity-Id", "X-Request-Id", "X-Source-Channel", "X-Purpose-Context", "Idempotency-Key"} {
			if r.Header.Get(h) == "" {
				http.Error(w, "envelope_incomplete: "+h, http.StatusBadRequest)
				return
			}
		}
		scope := tokenization.Scope{TenantID: r.Header.Get("X-Tenant-Id"), PrincipalID: r.Header.Get("X-Principal-Id"),
			LegalEntityID: r.Header.Get("X-Legal-Entity-Id")}
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

// principalAuthz grants payee actions by principal: everyone may administer
// and approve (authorization-svc's own-object SoD is kept: no self-approval);
// only privileged principals hold PAYEE_MASTER_PRIVILEGED_READ.
type principalAuthz struct{ privileged map[string]bool }

func (a principalAuthz) CheckAllowed(_ context.Context, principalID, _, action string) error {
	if action == handler.PayeeMasterPrivilegedRead && !a.privileged[principalID] {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

func (a principalAuthz) CheckAllowedOwnObject(_ context.Context, principalID, _, _, owner string) error {
	if principalID == owner {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

// TestE2E_TokenizedDestination_PaymentsReadPath proposes a destination with
// enforcement on, proves the database holds a token (not the identifier),
// walks it to ACTIVE, then reads it the way payment-authorization-svc does
// (/org10/parties/{party}/active) and by id.
func TestE2E_TokenizedDestination_PaymentsReadPath(t *testing.T) {
	pool := e2eDatabase(t)
	vault := vaultServer(t, map[string]bool{"bank-detail-reader": true})
	tenant, entity, party := uuid.NewString(), "le-e2e", "party-e2e"

	pgStore := store.NewPgStore(pool, zap.NewNop(), store.WithEventRegion("uk"),
		store.WithTokenization(tokenization.NewClient(vault.URL), true))
	parties := newStubParty()
	parties.add(party, entity)
	h := handler.New(pgStore, principalAuthz{privileged: map[string]bool{"bank-detail-reader": true, "half-privileged": true}},
		parties, zap.NewNop()).WithTokenizer(tokenization.NewClient(vault.URL))
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)

	send := func(method, path string, body any, principal string) *httptest.ResponseRecorder {
		return doRequestAsWithHeaders(r, method, path, body, tenant, principal, map[string]string{"X-Correlation-ID": "corr-e2e"})
	}

	// 1. Propose, enforcement on. The proposer is not privileged.
	w := send(http.MethodPost, "/org10/destinations", domain.ProposeDestinationRequest{
		LegalEntityID: entity, PartyRef: party, FinancialInstitution: "NatWest", AccountIdentifier: rawIBAN,
		CountryCode: "GB", Currency: "GBP", PayeeName: "E2E Payee", SourceType: domain.SourceManualEntry,
	}, "proposer")
	var proposed domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &proposed)
	if w.Code != http.StatusCreated || proposed.AccountIdentifier != "" || proposed.AccountLast4 != "6819" {
		t.Fatalf("propose: %d %s", w.Code, w.Body.String())
	}

	// 2. The database holds the token, never the identifier.
	var stored, token, source string
	if err := pool.QueryRow(context.Background(), `
		SELECT d.account_identifier, t.account_identifier_token, t.source
		FROM payee_destinations d JOIN payee_destination_identifier_tokens t USING (destination_id)
		WHERE d.destination_id = $1`, proposed.DestinationID).Scan(&stored, &token, &source); err != nil {
		t.Fatalf("token row: %v", err)
	}
	if stored == rawIBAN || stored != token || source != "WRITE" {
		t.Fatalf("stored account_identifier %q, token %q, source %q", stored, token, source)
	}

	// 3. Walk it to ACTIVE: verify, approve (a different principal), activate.
	for _, step := range []struct {
		path      string
		body      any
		principal string
	}{
		{"/verify", domain.VerifyDestinationRequest{VerificationMethod: "CALLBACK", VerificationEvidenceRef: "ev-1"}, "verifier"},
		{"/approve", nil, "approver"},
		{"/activate", nil, "approver"},
	} {
		if w := send(http.MethodPost, "/org10/destinations/"+proposed.DestinationID+step.path, step.body, step.principal); w.Code >= 300 {
			t.Fatalf("%s: %d %s", step.path, w.Code, w.Body.String())
		}
	}

	// 4. The payments read path and the by-id path, as a reader holding both
	// PAYEE_MASTER_PRIVILEGED_READ and TOKEN_DETOKENIZE: the REAL identifier.
	for _, path := range []string{"/org10/parties/" + party + "/active", "/org10/destinations/" + proposed.DestinationID} {
		w := send(http.MethodGet, path, nil, "bank-detail-reader")
		var d domain.PayeeDestination
		_ = json.Unmarshal(w.Body.Bytes(), &d)
		if w.Code != http.StatusOK || d.AccountIdentifier != rawIBAN || d.Status != domain.StatusActive || strings.Contains(w.Body.String(), token) {
			t.Fatalf("%s as bank-detail-reader: %d %s; want %s", path, w.Code, w.Body.String(), rawIBAN)
		}
	}

	// 5. Privileged at payee but WITHOUT TOKEN_DETOKENIZE at the vault: the
	// payments read path still answers 200 with the destination (eligibility
	// checks keep working), the identifier withheld — never the token.
	w = send(http.MethodGet, "/org10/parties/"+party+"/active", nil, "half-privileged")
	var half domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &half)
	if w.Code != http.StatusOK || half.AccountIdentifier != "" || half.Status != domain.StatusActive ||
		w.Header().Get(handler.HeaderIdentifierWithheld) != "detokenize_denied" || strings.Contains(w.Body.String(), token) {
		t.Fatalf("half-privileged: %d %s withheld=%q", w.Code, w.Body.String(), w.Header().Get(handler.HeaderIdentifierWithheld))
	}

	// 6. Not privileged: blank, last-4 only.
	w = send(http.MethodGet, "/org10/parties/"+party+"/active", nil, "ap-clerk")
	var clerk domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &clerk)
	if w.Code != http.StatusOK || clerk.AccountIdentifier != "" || clerk.AccountLast4 != "6819" || strings.Contains(w.Body.String(), token) {
		t.Fatalf("ap-clerk: %d %s", w.Code, w.Body.String())
	}
}

// A destination proposed raw before enforcement, then backfilled, reads back
// through the same handler exactly as a newly tokenized one (Phase 5.4).
func TestE2E_BackfilledDestinationReadsLikeANewOne(t *testing.T) {
	pool := e2eDatabase(t)
	vault := vaultServer(t, map[string]bool{"bank-detail-reader": true, "svc-tokenization-backfill": true})
	tenant, entity := uuid.NewString(), "le-e2e"
	parties := newStubParty()
	parties.add("party-legacy", entity)
	authz := principalAuthz{privileged: map[string]bool{"bank-detail-reader": true}}

	// Before enforcement: raw.
	raw := store.NewPgStore(pool, zap.NewNop(), store.WithEventRegion("uk"))
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(raw, authz, parties, zap.NewNop()))
	w := doRequestAsWithHeaders(r, http.MethodPost, "/org10/destinations", domain.ProposeDestinationRequest{
		LegalEntityID: entity, PartyRef: "party-legacy", FinancialInstitution: "NatWest", AccountIdentifier: rawIBAN,
		CountryCode: "GB", Currency: "GBP", PayeeName: "Legacy Payee", SourceType: domain.SourceManualEntry,
	}, tenant, "proposer", nil)
	var legacy domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &legacy)
	if w.Code != http.StatusCreated {
		t.Fatalf("legacy propose: %d %s", w.Code, w.Body.String())
	}

	// Backfill through the real HTTP client.
	report, err := store.BackfillIdentifierTokens(context.Background(), pool, tokenization.NewClient(vault.URL),
		store.BackfillOptions{PrincipalID: "svc-tokenization-backfill"})
	if err != nil || report.Tokenized != 1 {
		t.Fatalf("backfill: %+v, %v", report, err)
	}

	// After: the enforced service reads it exactly like a new one.
	enforced := store.NewPgStore(pool, zap.NewNop(), store.WithEventRegion("uk"),
		store.WithTokenization(tokenization.NewClient(vault.URL), true))
	r2 := chi.NewRouter()
	r2.Use(middleware.TenantContext())
	handler.RegisterRoutes(r2, handler.New(enforced, authz, parties, zap.NewNop()).WithTokenizer(tokenization.NewClient(vault.URL)))
	w = doRequestAsWithHeaders(r2, http.MethodGet, "/org10/destinations/"+legacy.DestinationID, nil, tenant, "bank-detail-reader", nil)
	var got domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.AccountIdentifier != rawIBAN {
		t.Fatalf("backfilled read: %d %s", w.Code, w.Body.String())
	}
	var rawColumn string
	_ = pool.QueryRow(context.Background(), `SELECT account_identifier FROM payee_destinations WHERE destination_id = $1`, legacy.DestinationID).Scan(&rawColumn)
	if rawColumn != rawIBAN {
		t.Fatalf("the backfill touched payee_destinations.account_identifier: %q", rawColumn)
	}
}
