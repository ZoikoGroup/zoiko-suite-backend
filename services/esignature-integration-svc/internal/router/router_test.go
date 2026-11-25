package router_test

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

	"go.uber.org/zap"

	"zoiko.io/esignature-integration-svc/internal/authz"
	"zoiko.io/esignature-integration-svc/internal/domain"
	"zoiko.io/esignature-integration-svc/internal/events"
	"zoiko.io/esignature-integration-svc/internal/handler"
	"zoiko.io/esignature-integration-svc/internal/idempotency"
	"zoiko.io/esignature-integration-svc/internal/provider"
	"zoiko.io/esignature-integration-svc/internal/router"
	"zoiko.io/esignature-integration-svc/internal/store"
)

// ── test doubles ────────────────────────────────────────────────────────────

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

// memIdem is an in-memory idempotency.Store with PgStore's semantics.
type memIdem struct {
	mu sync.Mutex
	m  map[string]*idempotency.Record
}

func newMemIdem() *memIdem { return &memIdem{m: map[string]*idempotency.Record{}} }
func (s *memIdem) Claim(_ context.Context, t, e, k, fp string) (*idempotency.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := t + "|" + e + "|" + k
	if r, ok := s.m[id]; ok {
		if r.RequestFingerprint != fp {
			return nil, idempotency.ErrFingerprintMismatch
		}
		c := *r
		return &c, nil
	}
	s.m[id] = &idempotency.Record{RequestFingerprint: fp}
	return nil, nil
}
func (s *memIdem) Complete(_ context.Context, t, e, k string, st int, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.m[t+"|"+e+"|"+k]
	r.ResponseStatus, r.ResponseBody = st, append([]byte(nil), b...)
	return nil
}
func (s *memIdem) Release(_ context.Context, t, e, k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.m[t+"|"+e+"|"+k]; ok && r.ResponseStatus == 0 {
		delete(s.m, t+"|"+e+"|"+k)
	}
	return nil
}

// policyAuthz stands in for authorization-svc: it denies exactly the actions
// in deny and grants everything else.
func policyAuthz(t *testing.T, deny ...string) *authz.Client {
	denied := map[string]bool{}
	for _, a := range deny {
		denied[a] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ActionType string `json:"action_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := "GRANTED"
		if denied[req.ActionType] {
			out = "DENIED"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": out})
	}))
	t.Cleanup(srv.Close)
	return authz.NewClient(srv.URL)
}

// notFoundOrch answers every orchestration query with not-found, so the
// contract test exercises routing without a database.
type notFoundOrch struct{}

var errNF = domain.ErrEnvelopeNotFound

func (notFoundOrch) CreateSignatureProfile(context.Context, *domain.CreateSignatureProfileRequest, string) (*domain.SignatureProfile, error) {
	return nil, domain.ErrSignatureProfileNotFound
}
func (notFoundOrch) GetSignatureProfile(context.Context, string) (*domain.SignatureProfile, error) {
	return nil, domain.ErrSignatureProfileNotFound
}
func (notFoundOrch) ActivateSignatureProfile(context.Context, string) (*domain.SignatureProfile, error) {
	return nil, domain.ErrSignatureProfileNotFound
}
func (notFoundOrch) RetireSignatureProfile(context.Context, string) (*domain.SignatureProfile, error) {
	return nil, domain.ErrSignatureProfileNotFound
}
func (notFoundOrch) BindSignatureProfile(context.Context, string, string) (*domain.SignatureEnvelope, error) {
	return nil, errNF
}
func (notFoundOrch) AmendEnvelope(context.Context, string, *domain.CreateEnvelopeRequest, string) (*domain.SignatureEnvelope, error) {
	return nil, errNF
}
func (notFoundOrch) RecordProviderAttempt(context.Context, string, *domain.RecordProviderAttemptRequest) (*domain.ProviderAttempt, error) {
	return nil, errNF
}
func (notFoundOrch) ReconcileAttempt(context.Context, string, *domain.ReconcileAttemptRequest, string) (*domain.ProviderAttempt, error) {
	return nil, domain.ErrProviderAttemptNotFound
}
func (notFoundOrch) ListProviderAttempts(context.Context, string) ([]domain.ProviderAttempt, error) {
	return nil, nil
}
func (notFoundOrch) AddParticipant(context.Context, string, *domain.AddParticipantRequest) (*domain.Participant, error) {
	return nil, errNF
}
func (notFoundOrch) MarkParticipantViewed(context.Context, string) (*domain.Participant, error) {
	return nil, domain.ErrParticipantNotFound
}
func (notFoundOrch) MarkParticipantSigned(context.Context, string) (*domain.Participant, error) {
	return nil, domain.ErrParticipantNotFound
}
func (notFoundOrch) DeclineParticipant(context.Context, string, string) (*domain.Participant, error) {
	return nil, domain.ErrParticipantNotFound
}
func (notFoundOrch) ListParticipants(context.Context, string) ([]domain.Participant, error) {
	return nil, nil
}
func (notFoundOrch) SealCompletionEvidence(context.Context, string, *domain.SealCompletionEvidenceRequest, string) (*domain.CompletionEvidence, error) {
	return nil, errNF
}
func (notFoundOrch) GetCompletionEvidence(context.Context, string) (*domain.CompletionEvidence, error) {
	return nil, errors.New("completion evidence not found")
}

var _ store.SignatureOrchestrationStore = notFoundOrch{}

type okProvider struct{}

func (okProvider) Name() string { return domain.ProviderDocuSign }
func (okProvider) Send(_ context.Context, r provider.SendRequest) (provider.SendResult, error) {
	return provider.SendResult{ExternalRef: "ref-" + r.EnvelopeID}, nil
}

func newRouter(t *testing.T, db pinger, az *authz.Client, reg *provider.Registry) http.Handler {
	st := store.NewMemoryStore()
	pub := events.NewMockPublisher()
	h := handler.New(st, pub, az, zap.NewNop()).WithProviders(reg)
	oh := handler.NewOrchestrationHandler(notFoundOrch{}, st, pub, az, zap.NewNop()).WithProviders(reg)
	return router.New(router.Deps{DB: db, Idem: newMemIdem(), Handler: h, Orchestration: oh, Logger: zap.NewNop()})
}

var keySeq int

// call sends a request carrying a complete canonical envelope.
func call(h http.Handler, method, path, tenant string, body any) *httptest.ResponseRecorder {
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", "principal-01")
	req.Header.Set("X-Request-Id", "req-1")
	req.Header.Set("X-Correlation-ID", "corr-1")
	req.Header.Set("X-Source-Channel", "api")
	req.Header.Set("X-Legal-Entity-Id", "le-1")
	keySeq++
	req.Header.Set("Idempotency-Key", "key-"+method+path+string(rune('a'+keySeq%26))+strings.Repeat("x", keySeq%7))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// routerMiss reports whether the router itself (not a handler) refused the
// request: chi's default 404 is text/plain "404 page not found", every
// handler 404 in this service is JSON.
func routerMiss(w *httptest.ResponseRecorder) bool {
	if w.Code == http.StatusMethodNotAllowed {
		return true
	}
	return w.Code == http.StatusNotFound && strings.Contains(w.Body.String(), "404 page not found")
}

// ── tests ───────────────────────────────────────────────────────────────────

func TestProbes_PublicAndReadyzReflectsDB(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t), provider.NewRegistry())
	for _, p := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil)) // no identity headers
		if w.Code != http.StatusOK {
			t.Fatalf("%s without headers: want 200, got %d: %s", p, w.Code, w.Body.String())
		}
	}
	down := newRouter(t, pinger{err: errors.New("db down")}, policyAuthz(t), provider.NewRegistry())
	w := httptest.NewRecorder()
	down.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"unavailable"`) {
		t.Fatalf("readyz with DB down: want 503 unavailable, got %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	down.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("healthz is liveness and must not depend on the DB, got %d", w.Code)
	}
}

// TestRouteContract hits every route this service documents and asserts the
// router serves it (not 404/405 from chi).
func TestRouteContract(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t), provider.NewRegistry(okProvider{}))
	create := call(h, "POST", "/v1/esignature/envelopes", "tenant-a", domain.CreateEnvelopeRequest{
		LegalEntityID: "le-1", Provider: domain.ProviderDocuSign, DocumentTitle: "MSA", SignerEmail: "s@example.com", SignerName: "S"})
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	var env domain.SignatureEnvelope
	_ = json.Unmarshal(create.Body.Bytes(), &env)
	id := env.EnvelopeID

	routes := []struct{ method, path string }{
		{"GET", "/v1/esignature/envelopes"},
		{"GET", "/v1/esignature/envelopes/" + id},
		{"POST", "/v1/esignature/envelopes/" + id + "/status"},
		{"PATCH", "/v1/esignature/envelopes/" + id + "/status"},
		{"POST", "/v1/esignature/profiles"},
		{"POST", "/v1/esignature/profiles/"},
		{"GET", "/v1/esignature/profiles/p-1"},
		{"POST", "/v1/esignature/profiles/p-1/activate"},
		{"POST", "/v1/esignature/profiles/p-1/retire"},
		{"POST", "/v1/esignature/envelopes/" + id + "/bind-profile"},
		{"POST", "/v1/esignature/envelopes/" + id + "/amend"},
		{"POST", "/v1/esignature/envelopes/" + id + "/attempts"},
		{"GET", "/v1/esignature/envelopes/" + id + "/attempts"},
		{"POST", "/v1/esignature/envelopes/" + id + "/participants"},
		{"GET", "/v1/esignature/envelopes/" + id + "/participants"},
		{"POST", "/v1/esignature/envelopes/" + id + "/completion-evidence"},
		{"GET", "/v1/esignature/envelopes/" + id + "/completion-evidence"},
		{"POST", "/v1/esignature/attempts/a-1/reconcile"},
		{"POST", "/v1/esignature/participants/p-1/mark-viewed"},
		{"POST", "/v1/esignature/participants/p-1/mark-signed"},
		{"POST", "/v1/esignature/participants/p-1/decline"},
	}
	for _, rt := range routes {
		w := call(h, rt.method, rt.path, "tenant-a", map[string]string{})
		if routerMiss(w) {
			t.Errorf("%s %s: router answered %d (%s)", rt.method, rt.path, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
}

func TestAuthzDeny_403(t *testing.T) {
	// Seed an envelope with a granting policy, then deny status updates.
	reg := provider.NewRegistry(okProvider{})
	h := newRouter(t, pinger{}, policyAuthz(t, handler.ENVELOPE_CREATE), reg)
	w := call(h, "POST", "/v1/esignature/envelopes", "tenant-a", domain.CreateEnvelopeRequest{
		LegalEntityID: "le-1", Provider: domain.ProviderDocuSign, DocumentTitle: "MSA", SignerEmail: "s@example.com", SignerName: "S"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("create with ENVELOPE_CREATE denied: want 403, got %d: %s", w.Code, w.Body.String())
	}

	h = newRouter(t, pinger{}, policyAuthz(t, handler.PROFILE_CREATE), reg)
	w = call(h, "POST", "/v1/esignature/profiles", "tenant-a", domain.CreateSignatureProfileRequest{LegalEntityID: "le-1"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("profile create denied: want 403, got %d: %s", w.Code, w.Body.String())
	}
	// Allowed path through the same policy stub.
	w = call(h, "POST", "/v1/esignature/envelopes", "tenant-a", domain.CreateEnvelopeRequest{
		LegalEntityID: "le-1", Provider: domain.ProviderDocuSign, DocumentTitle: "MSA", SignerEmail: "s@example.com", SignerName: "S"})
	if w.Code != http.StatusCreated {
		t.Fatalf("allowed action: want 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestMissingIdentity_FailsClosed(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t), provider.NewRegistry(okProvider{}))
	req := httptest.NewRequest("GET", "/v1/esignature/envelopes", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no tenant: want 401, got %d", w.Code)
	}
	req = httptest.NewRequest("POST", "/v1/esignature/envelopes", strings.NewReader(`{}`))
	req.Header.Set("X-Tenant-Id", "tenant-a")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("write without principal: want 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCrossTenant_EnvelopeInvisibleAndImmutable(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t), provider.NewRegistry(okProvider{}))
	w := call(h, "POST", "/v1/esignature/envelopes", "tenant-a", domain.CreateEnvelopeRequest{
		LegalEntityID: "le-1", Provider: domain.ProviderDocuSign, DocumentTitle: "MSA", SignerEmail: "s@example.com", SignerName: "S"})
	var env domain.SignatureEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if g := call(h, "GET", "/v1/esignature/envelopes/"+env.EnvelopeID, "tenant-b", nil); g.Code != http.StatusNotFound {
		t.Fatalf("tenant-b read: want 404, got %d", g.Code)
	}
	if u := call(h, "POST", "/v1/esignature/envelopes/"+env.EnvelopeID+"/status", "tenant-b",
		domain.UpdateStatusRequest{Status: domain.EnvelopeVoided}); u.Code != http.StatusNotFound {
		t.Fatalf("tenant-b void: want 404, got %d", u.Code)
	}
	if g := call(h, "GET", "/v1/esignature/envelopes/"+env.EnvelopeID, "tenant-a", nil); !strings.Contains(g.Body.String(), `"SENT"`) {
		t.Fatalf("tenant-a envelope must be unchanged: %s", g.Body.String())
	}
}

func TestIdempotentReplay_CreateEnvelope(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t), provider.NewRegistry(okProvider{}))
	body := `{"legal_entity_id":"le-1","provider":"DOCUSIGN","document_title":"MSA","signer_email":"s@example.com","signer_name":"S"}`
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/esignature/envelopes", strings.NewReader(body))
		for k, v := range map[string]string{"X-Tenant-Id": "tenant-a", "X-Principal-Id": "p", "X-Request-Id": "r",
			"X-Correlation-ID": "c", "X-Source-Channel": "api", "X-Legal-Entity-Id": "le-1", "Idempotency-Key": "same-key"} {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	first, second := send(), send()
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("codes %d %d", first.Code, second.Code)
	}
	if second.Header().Get("X-Idempotent-Replay") != "true" || first.Body.String() != second.Body.String() {
		t.Fatalf("second call must replay the first answer")
	}
	list := call(h, "GET", "/v1/esignature/envelopes", "tenant-a", nil)
	if !strings.Contains(list.Body.String(), `"total":1`) {
		t.Fatalf("replay must not create a second envelope: %s", list.Body.String())
	}
}
