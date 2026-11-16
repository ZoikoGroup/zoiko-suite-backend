package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/key-management-svc/internal/authz"
	kmscrypto "zoiko.io/key-management-svc/internal/crypto"
	"zoiko.io/key-management-svc/internal/handler"
	"zoiko.io/key-management-svc/internal/siem"
	"zoiko.io/key-management-svc/internal/store"
	"zoiko.io/key-management-svc/internal/testpg"
)

const (
	rawIBAN  = "GB29NWBK60161331926819"
	vaultKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
)

// siemEvent is one event the vault streamed, as siem-integration-svc saw it.
type siemEvent struct {
	Tenant    string
	EventType string `json:"event_type"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
}

// fakeSIEM is a siem-integration-svc with one ACTIVE exporter per tenant, so
// every event this service streams is recorded.
type fakeSIEM struct {
	mu     sync.Mutex
	events []siemEvent
	srv    *httptest.Server
}

func newFakeSIEM(t *testing.T) *fakeSIEM {
	f := &fakeSIEM{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/siem/exporters":
			_, _ = w.Write([]byte(`{"data":[{"id":"exp-1","status":"ACTIVE"}]}`))
		case "/v1/siem/stream":
			var e siemEvent
			_ = json.NewDecoder(r.Body).Decode(&e)
			e.Tenant = r.Header.Get("X-Tenant-ID")
			f.mu.Lock()
			f.events = append(f.events, e)
			f.mu.Unlock()
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSIEM) all() []siemEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]siemEvent(nil), f.events...)
}

// recordingAuthz grants everything except the actions in deny, and records
// every check so a test can see which legal entity was authorized.
type recordingAuthz struct {
	mu     sync.Mutex
	deny   map[string]error
	checks []string // "ACTION@legal-entity"
}

func (a *recordingAuthz) CheckAllowed(_ context.Context, _, legalEntityID, action string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checks = append(a.checks, action+"@"+legalEntityID)
	return a.deny[action]
}

// vaultEnv is one router with the vault wired, its SIEM sink and its authz.
type vaultEnv struct {
	router http.Handler
	siem   *siem.Client
	sink   *fakeSIEM
	authz  *recordingAuthz
}

// drain waits for every streamed event to reach the fake SIEM.
func (e *vaultEnv) drain() []siemEvent {
	e.siem.Close()
	return e.sink.all()
}

// storeVault is what PgStore and MemoryStore both implement.
type storeVault interface {
	store.Store
	store.Vault
}

// vaultBackends runs a test against MemoryStore and, with TEST_DATABASE_URL
// set, against PgStore — the same handler code over both vaults.
func vaultBackends(t *testing.T, test func(t *testing.T, newEnv func() *vaultEnv)) {
	for name, open := range map[string]func(t *testing.T) storeVault{
		"memory":   func(*testing.T) storeVault { return store.NewMemoryStore() },
		"postgres": func(t *testing.T) storeVault { return store.NewPgStore(testpg.Open(t, "kms_handler_test").App) },
	} {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			cipher, err := kmscrypto.New(vaultKey)
			if err != nil {
				t.Fatal(err)
			}
			test(t, func() *vaultEnv {
				sink := newFakeSIEM(t)
				sc := siem.New(sink.srv.URL, "key-management-svc", zap.NewNop())
				az := &recordingAuthz{deny: map[string]error{}}
				return &vaultEnv{
					router: handler.NewRouter(handler.New(s, sc, az, zap.NewNop()).WithVault(s, cipher)),
					siem:   sc, sink: sink, authz: az,
				}
			})
		})
	}
}

// clientHeaders is exactly what banking-connector-svc's and
// payee-banking-identity-svc's internal/tokenization clients send: the
// contract's three headers plus the platform envelope (§4) every service
// enforces on POSTs in its default write-strict mode.
func clientHeaders(r *http.Request, tenant, principal, legalEntity string) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", tenant)
	r.Header.Set("X-Principal-Id", principal)
	r.Header.Set("X-Correlation-ID", "corr-"+principal)
	r.Header.Set("X-Legal-Entity-Id", legalEntity)
	r.Header.Set("X-Request-Id", uuid.NewString())
	r.Header.Set("X-Source-Channel", "system")
	r.Header.Set("X-Purpose-Context", "bank_detail_tokenization")
	r.Header.Set("Idempotency-Key", uuid.NewString())
}

func post(router http.Handler, path string, body any, headers func(*http.Request)) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	headers(r)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

func tokenize(t *testing.T, e *vaultEnv, tenant, value string) string {
	t.Helper()
	w := post(e.router, "/v1/tokenize", map[string]string{"value": value, "classification": "S3_RESTRICTED"},
		func(r *http.Request) { clientHeaders(r, tenant, "svc-principal", "le-1") })
	if w.Code != http.StatusOK {
		t.Fatalf("tokenize: %d %s", w.Code, w.Body.String())
	}
	var out struct{ Token string }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if _, err := uuid.Parse(out.Token); err != nil {
		t.Fatalf("token %q is not a UUID", out.Token)
	}
	return out.Token
}

func detokenize(e *vaultEnv, tenant, principal, token string) *httptest.ResponseRecorder {
	return post(e.router, "/v1/detokenize", map[string]string{"token": token},
		func(r *http.Request) { clientHeaders(r, tenant, principal, "le-1") })
}

// Brief item 9: tokenize then detokenize returns exactly the original value.
func TestVault_RoundTripReturnsTheExactValue(t *testing.T) {
	vaultBackends(t, func(t *testing.T, newEnv func() *vaultEnv) {
		for _, raw := range []string{rawIBAN, " 0012 3456 ", "ünïcødé-Konto-Nr.\t42", strings.Repeat("9", 1024)} {
			e := newEnv()
			tenant := uuid.NewString()
			tok := tokenize(t, e, tenant, raw)
			if tok == raw || strings.Contains(tok, raw) {
				t.Fatal("the token carries the value")
			}
			w := detokenize(e, tenant, "auditor", tok)
			var out struct{ Value string }
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			if w.Code != http.StatusOK || out.Value != raw {
				t.Fatalf("detokenize = %d %q, want exactly %q", w.Code, out.Value, raw)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("a raw value was served cacheable")
			}
			events := e.drain()
			created, detok := ofType(events, "token.created"), ofType(events, "token.detokenized")
			if len(events) != 2 || len(created) != 1 || created[0].Severity != "MEDIUM" ||
				len(detok) != 1 || detok[0].Severity != "HIGH" || detok[0].Tenant != tenant ||
				!strings.Contains(detok[0].Message, "succeeded") || !strings.Contains(detok[0].Message, "principal=auditor") {
				t.Fatalf("SIEM events: %+v", events)
			}
			for _, ev := range events {
				if strings.Contains(ev.Message, raw) {
					t.Fatalf("the raw value reached SIEM: %q", ev.Message)
				}
			}
		}
	})
}

// Brief item 10 (SEC-T29): tenant A's token presented by tenant B is refused
// with 403 — the same 403 as a token that never existed, so B learns nothing
// about A — and the attempt is recorded under B.
func TestVault_CrossTenantDetokenizeIsRefusedAndRecorded(t *testing.T) {
	vaultBackends(t, func(t *testing.T, newEnv func() *vaultEnv) {
		e := newEnv()
		tenantA, tenantB := uuid.NewString(), uuid.NewString()
		tok := tokenize(t, e, tenantA, rawIBAN)

		cross := detokenize(e, tenantB, "intruder", tok)
		if cross.Code != http.StatusForbidden || strings.Contains(cross.Body.String(), rawIBAN) {
			t.Fatalf("cross-tenant detokenize = %d %s", cross.Code, cross.Body.String())
		}
		missing := detokenize(e, tenantB, "intruder", uuid.NewString())
		if missing.Code != cross.Code || problemDetail(t, missing) != problemDetail(t, cross) {
			t.Fatalf("a missing token (%d %q) is distinguishable from another tenant's (%d %q)",
				missing.Code, problemDetail(t, missing), cross.Code, problemDetail(t, cross))
		}
		for _, c := range e.authz.checks {
			if strings.HasPrefix(c, handler.TokenDetokenize) {
				t.Fatalf("authorization was consulted for a token the tenant cannot see: %v", e.authz.checks)
			}
		}
		// Delivery is concurrent, so find the attempt on tenant A's token
		// rather than assuming an order.
		recorded := ofType(e.drain(), "token.detokenized")
		var crossEvent *siemEvent
		for i := range recorded {
			if strings.Contains(recorded[i].Message, "token="+tok) {
				crossEvent = &recorded[i]
			}
		}
		if len(recorded) != 2 || crossEvent == nil || crossEvent.Tenant != tenantB || crossEvent.Severity != "HIGH" ||
			!strings.Contains(crossEvent.Message, "refused:not_available_to_tenant") ||
			!strings.Contains(crossEvent.Message, "principal=intruder") {
			t.Fatalf("attempts recorded: %+v", recorded)
		}
	})
}

// Brief item 11 (SEC-T15): without TOKEN_DETOKENIZE the request is refused
// and nothing is returned — even though the tenant matches and the token
// exists — and the refusal is recorded. Authorization is checked at the
// legal entity stored with the token, not the one the caller sends.
func TestVault_DetokenizeRequiresTheElevatedGrant(t *testing.T) {
	vaultBackends(t, func(t *testing.T, newEnv func() *vaultEnv) {
		for name, tc := range map[string]struct {
			err     error
			status  int
			outcome string
		}{
			"denied":                    {authz.ErrAuthorizationDenied, http.StatusForbidden, "refused:not_authorized"},
			"authorization unavailable": {authz.ErrAuthzServiceUnavailable, http.StatusServiceUnavailable, "refused:authorization_unavailable"},
		} {
			t.Run(name, func(t *testing.T) {
				e := newEnv()
				tenant := uuid.NewString()
				tok := tokenize(t, e, tenant, rawIBAN)
				e.authz.deny[handler.TokenDetokenize] = tc.err

				w := post(e.router, "/v1/detokenize", map[string]string{"token": tok}, func(r *http.Request) {
					clientHeaders(r, tenant, "support-agent", "le-1")
					r.Header.Set("X-Legal-Entity-Id", "le-the-caller-claims")
				})
				if w.Code != tc.status || strings.Contains(w.Body.String(), rawIBAN) || strings.Contains(w.Body.String(), `"value"`) {
					t.Fatalf("detokenize = %d %s", w.Code, w.Body.String())
				}
				if last := e.authz.checks[len(e.authz.checks)-1]; last != handler.TokenDetokenize+"@le-1" {
					t.Fatalf("authorized %q, want TOKEN_DETOKENIZE at the token's own legal entity le-1", last)
				}
				events := e.drain()
				if detok := ofType(events, "token.detokenized"); len(detok) != 1 || !strings.Contains(detok[0].Message, tc.outcome) {
					t.Fatalf("refusal not recorded: %+v", events)
				}
			})
		}
	})
}

// Tokenize is a material write: authorized on TOKEN_CREATE at the caller's
// legal entity, and refused for any class the vault must not hold.
func TestVault_TokenizeIsAuthorizedAndValidated(t *testing.T) {
	vaultBackends(t, func(t *testing.T, newEnv func() *vaultEnv) {
		e := newEnv()
		tenant := uuid.NewString()
		tokenize(t, e, tenant, rawIBAN)
		if e.authz.checks[0] != handler.TokenCreate+"@le-1" {
			t.Fatalf("tokenize authorized %v, want TOKEN_CREATE@le-1", e.authz.checks)
		}

		for _, class := range []string{"", "S4_CROWN_JEWEL", "S1_INTERNAL", "s3_restricted", "SECRET"} {
			w := post(e.router, "/v1/tokenize", map[string]string{"value": rawIBAN, "classification": class},
				func(r *http.Request) { clientHeaders(r, tenant, "svc-principal", "le-1") })
			if w.Code != http.StatusBadRequest {
				t.Fatalf("classification %q accepted: %d", class, w.Code)
			}
		}
		for name, body := range map[string]map[string]string{
			"no value":       {"classification": "S3_RESTRICTED"},
			"value too long": {"value": strings.Repeat("9", 1025), "classification": "S3_RESTRICTED"},
		} {
			if w := post(e.router, "/v1/tokenize", body, func(r *http.Request) { clientHeaders(r, tenant, "p", "le-1") }); w.Code != http.StatusBadRequest {
				t.Fatalf("%s: %d", name, w.Code)
			}
		}

		e.authz.deny[handler.TokenCreate] = authz.ErrAuthorizationDenied
		if w := post(e.router, "/v1/tokenize", map[string]string{"value": rawIBAN, "classification": "S3_RESTRICTED"},
			func(r *http.Request) { clientHeaders(r, tenant, "p", "le-1") }); w.Code != http.StatusForbidden {
			t.Fatalf("tokenize without TOKEN_CREATE: %d", w.Code)
		}
		if created := len(ofType(e.drain(), "token.created")); created != 1 {
			t.Fatalf("%d token.created events; only the first, valid request created a token", created)
		}
	})
}

// INV-08: a retried tokenize returns the original token; the same key for a
// different value is a conflict, never a second value under the first token.
func TestVault_TokenizeIsIdempotent(t *testing.T) {
	vaultBackends(t, func(t *testing.T, newEnv func() *vaultEnv) {
		e := newEnv()
		tenant := uuid.NewString()
		send := func(value string) *httptest.ResponseRecorder {
			return post(e.router, "/v1/tokenize", map[string]string{"value": value, "classification": "S3_RESTRICTED"},
				func(r *http.Request) {
					clientHeaders(r, tenant, "svc-principal", "le-1")
					r.Header.Set("Idempotency-Key", "retry-key")
				})
		}
		first, again := send(rawIBAN), send(rawIBAN)
		if first.Code != http.StatusOK || again.Code != http.StatusOK || first.Body.String() != again.Body.String() {
			t.Fatalf("retry: %d %s / %d %s", first.Code, first.Body.String(), again.Code, again.Body.String())
		}
		if w := send("GB82WEST12345698765432"); w.Code != http.StatusConflict {
			t.Fatalf("same key, different value: %d %s", w.Code, w.Body.String())
		}
	})
}

// The contract as the two clients send it passes this service's envelope
// middleware in its default write-strict mode — and the three headers the
// clients originally sent alone would not have.
func TestVault_ClientRequestsPassTheEnvelope(t *testing.T) {
	vaultBackends(t, func(t *testing.T, newEnv func() *vaultEnv) {
		e := newEnv()
		tenant := uuid.NewString()
		tok := tokenize(t, e, tenant, rawIBAN) // clientHeaders: the full envelope
		if w := detokenize(e, tenant, "auditor", tok); w.Code != http.StatusOK {
			t.Fatalf("detokenize with the client's headers: %d %s", w.Code, w.Body.String())
		}
		w := post(e.router, "/v1/tokenize", map[string]string{"value": rawIBAN, "classification": "S3_RESTRICTED"},
			func(r *http.Request) {
				r.Header.Set("X-Tenant-Id", tenant)
				r.Header.Set("X-Principal-Id", "svc-principal")
				r.Header.Set("X-Correlation-ID", "corr")
			})
		if w.Code != http.StatusBadRequest || w.Header().Get("X-Envelope-Contract") != "violated" {
			t.Fatalf("contract-only headers: %d %s; want the envelope refusal", w.Code, w.Body.String())
		}
	})
}

// A router built without WithVault refuses instead of tokenizing into nowhere.
func TestVault_UnconfiguredVaultRefuses(t *testing.T) {
	r := handler.NewRouter(handler.New(store.NewMemoryStore(), siem.New("", "key-management-svc", zap.NewNop()), &stubAuthz{}, zap.NewNop()))
	w := post(r, "/v1/tokenize", map[string]string{"value": rawIBAN, "classification": "S3_RESTRICTED"},
		func(req *http.Request) { clientHeaders(req, uuid.NewString(), "p", "le-1") })
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("tokenize with no vault: %d", w.Code)
	}
}

func ofType(events []siemEvent, eventType string) []siemEvent {
	var out []siemEvent
	for _, ev := range events {
		if ev.EventType == eventType {
			out = append(out, ev)
		}
	}
	return out
}

func problemDetail(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct{ Detail string }
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	return p.Detail
}
