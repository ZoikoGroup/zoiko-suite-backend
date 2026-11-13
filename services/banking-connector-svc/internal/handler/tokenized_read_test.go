package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
	"zoiko.io/banking-connector-svc/internal/tokenization"
)

const (
	readTenant = "tenant-tokread"
	readEntity = "le-tokread"
	rawNumber  = "GB29NWBK60161331926819"
)

// recordingTokenizer wraps the Fake, recording each detokenize scope, and
// can be told to fail detokenization.
type recordingTokenizer struct {
	*tokenization.Fake
	mu        sync.Mutex
	scopes    []tokenization.Scope
	detokFail error
}

func (r *recordingTokenizer) Detokenize(ctx context.Context, scope tokenization.Scope, token string) (string, error) {
	r.mu.Lock()
	r.scopes = append(r.scopes, scope)
	fail := r.detokFail
	r.mu.Unlock()
	if fail != nil {
		return "", fail
	}
	return r.Fake.Detokenize(ctx, scope, token)
}

// actionAuthz answers /v1/authorize per action: GRANTED unless the action is
// in deny. It records every action it was asked about.
func actionAuthz(t *testing.T, deny map[string]bool, asked *[]string) *authz.Client {
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		*asked = append(*asked, body["action_type"]+"@"+body["legal_entity_id"])
		mu.Unlock()
		outcome := "GRANTED"
		if deny[body["action_type"]] {
			outcome = "DENIED"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": outcome})
	}))
	t.Cleanup(srv.Close)
	return authz.NewClient(srv.URL)
}

type readFixture struct {
	router       chi.Router
	tok          *recordingTokenizer
	asked        []string
	tokenizedID  string
	legacyID     string
	storedToken  string
	storedMasked string
}

// newReadFixture seeds one TOKENIZED connection (a token row, as an enforced
// write or the backfill leaves it: account_number holds the token) and one
// LEGACY raw connection, and wires a router with the given tokenizer state.
func newReadFixture(t *testing.T, deny map[string]bool, withTokenizer bool) *readFixture {
	t.Helper()
	f := &readFixture{tok: &recordingTokenizer{Fake: &tokenization.Fake{}}}
	st := store.NewMemoryStore()
	ctx := middleware.WithTenant(context.Background(), readTenant)

	scope := tokenization.Scope{TenantID: readTenant, PrincipalID: "writer", LegalEntityID: readEntity}
	token, err := f.tok.Tokenize(ctx, scope, tokenization.ClassificationRestricted, rawNumber)
	if err != nil {
		t.Fatal(err)
	}
	f.storedToken, f.storedMasked = token, domain.MaskAccountNumber(rawNumber)
	tokenized := &domain.BankConnection{LegalEntityID: readEntity, BankName: "Token Bank", AccountNumber: token, IBAN: token,
		AccountNumberToken: token, AccountNumberMasked: f.storedMasked, Currency: "GBP", Status: domain.StatusConnected}
	legacy := &domain.BankConnection{LegalEntityID: readEntity, BankName: "Legacy Bank", AccountNumber: "GB00TEST1234567890",
		IBAN: "GB00TEST1234567890", Currency: "GBP", Status: domain.StatusConnected}
	for _, c := range []*domain.BankConnection{tokenized, legacy} {
		if err := st.CreateConnection(ctx, c, "writer", "corr"); err != nil {
			t.Fatal(err)
		}
	}
	f.tokenizedID, f.legacyID = tokenized.ConnectionID, legacy.ConnectionID

	h := New(st, actionAuthz(t, deny, &f.asked), zap.NewNop())
	if withTokenizer {
		h.WithTokenizer(f.tok)
	}
	f.router = chi.NewRouter()
	f.router.Use(middleware.TenantContext)
	RegisterRoutes(f.router, h)
	return f
}

func (f *readFixture) get(path, principal string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Tenant-Id", readTenant)
	req.Header.Set("X-Principal-Id", principal)
	req.Header.Set("X-Correlation-ID", "corr-read")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

// The masked view of a tokenized connection shows the REAL number's last 4
// (from the stored masked form) — not the token's last characters — and
// never calls the vault. The token and the raw number appear nowhere.
func TestTokenizedRead_MaskedViewUsesStoredMaskedForm(t *testing.T) {
	f := newReadFixture(t, nil, true)
	want := "******************6819"
	if f.storedMasked != want {
		t.Fatalf("fixture: masked %q", f.storedMasked)
	}
	for _, path := range []string{"/v1/banking/connections/" + f.tokenizedID, "/v1/banking/accounts/" + f.tokenizedID, "/v1/banking/connections"} {
		w := f.get(path, "any-reader")
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("%s: %d %s; want the masked form %s", path, w.Code, body, want)
		}
		if strings.Contains(body, f.storedToken) || strings.Contains(body, rawNumber) {
			t.Fatalf("%s leaked the token or the raw number: %s", path, body)
		}
		if maskedToken := domain.MaskAccountNumber(f.storedToken); strings.Contains(body, maskedToken) {
			t.Fatalf("%s masked the TOKEN's characters as if they were account digits: %s", path, body)
		}
	}
	if f.tok.Calls() != 1 { // the fixture's own Tokenize
		t.Fatalf("the masked view called the vault (%d calls)", f.tok.Calls()-1)
	}
	// The legacy row is masked exactly as before.
	if w := f.get("/v1/banking/connections/"+f.legacyID, "any-reader"); !strings.Contains(w.Body.String(), "**************7890") {
		t.Fatalf("legacy masked view changed: %s", w.Body.String())
	}
}

// The privileged route returns the REAL number of a tokenized connection,
// detokenized under the caller's own principal at the row's legal entity,
// after TOKEN_DETOKENIZE is granted here too.
func TestAccountNumber_TokenizedIsDetokenizedForAGrantedReader(t *testing.T) {
	f := newReadFixture(t, nil, true)
	w := f.get("/v1/banking/connections/"+f.tokenizedID+"/account-number", "bank-detail-reader")
	var got accountNumberResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.AccountNumber != rawNumber || got.IBAN != rawNumber || !got.Tokenized {
		t.Fatalf("account-number: %d %s; want the raw number %s", w.Code, w.Body.String(), rawNumber)
	}
	if strings.Contains(w.Body.String(), f.storedToken) {
		t.Fatal("the token was returned")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("a raw number was served cacheable")
	}
	if len(f.asked) != 1 || f.asked[0] != TOKEN_DETOKENIZE+"@"+readEntity {
		t.Fatalf("authorization asked %v; want TOKEN_DETOKENIZE at the row's legal entity", f.asked)
	}
	if len(f.tok.scopes) != 1 {
		t.Fatalf("detokenize calls: %d", len(f.tok.scopes))
	}
	s := f.tok.scopes[0]
	if s.PrincipalID != "bank-detail-reader" || s.TenantID != readTenant || s.LegalEntityID != readEntity || s.CorrelationID != "corr-read" {
		t.Fatalf("detokenized under %+v; want the caller's own principal, tenant and the row's legal entity", s)
	}
}

// Every refusal is fail-closed: no number, no token, and no vault call when
// this service's own check already refused.
func TestAccountNumber_FailsClosed(t *testing.T) {
	cases := map[string]struct {
		deny          map[string]bool
		withTokenizer bool
		detokFail     error
		id            func(f *readFixture) string
		want          int
		vaultCalled   bool
	}{
		"no TOKEN_DETOKENIZE, tokenized": {deny: map[string]bool{TOKEN_DETOKENIZE: true}, withTokenizer: true,
			id: func(f *readFixture) string { return f.tokenizedID }, want: http.StatusForbidden},
		"no TOKEN_DETOKENIZE, legacy raw row": {deny: map[string]bool{TOKEN_DETOKENIZE: true}, withTokenizer: true,
			id: func(f *readFixture) string { return f.legacyID }, want: http.StatusForbidden},
		"vault denies": {withTokenizer: true, detokFail: tokenization.ErrDetokenizationDenied,
			id: func(f *readFixture) string { return f.tokenizedID }, want: http.StatusForbidden, vaultCalled: true},
		"vault unavailable": {withTokenizer: true, detokFail: tokenization.ErrTokenizationServiceUnavailable,
			id: func(f *readFixture) string { return f.tokenizedID }, want: http.StatusServiceUnavailable, vaultCalled: true},
		"no tokenizer configured": {withTokenizer: false,
			id: func(f *readFixture) string { return f.tokenizedID }, want: http.StatusServiceUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newReadFixture(t, tc.deny, tc.withTokenizer)
			f.tok.detokFail = tc.detokFail
			w := f.get("/v1/banking/connections/"+tc.id(f)+"/account-number", "someone")
			body := w.Body.String()
			if w.Code != tc.want || strings.Contains(body, rawNumber) || strings.Contains(body, f.storedToken) ||
				strings.Contains(body, "GB00TEST1234567890") {
				t.Fatalf("%d %s; want %d and no number or token", w.Code, body, tc.want)
			}
			if called := len(f.tok.scopes) > 0; called != tc.vaultCalled {
				t.Fatalf("vault called = %v, want %v", called, tc.vaultCalled)
			}
		})
	}
}

// A legacy (raw, not yet backfilled) row is returned as stored once the
// grant is held, without a vault call.
func TestAccountNumber_LegacyRowNeedsNoVault(t *testing.T) {
	f := newReadFixture(t, nil, true)
	w := f.get("/v1/banking/connections/"+f.legacyID+"/account-number", "bank-detail-reader")
	var got accountNumberResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.AccountNumber != "GB00TEST1234567890" || got.Tokenized || len(f.tok.scopes) != 0 {
		t.Fatalf("legacy: %d %s (vault calls %d)", w.Code, w.Body.String(), len(f.tok.scopes))
	}
}
