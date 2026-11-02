package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"zoiko.io/compliance-risk-scoring-svc/internal/authz"
	"zoiko.io/compliance-risk-scoring-svc/internal/handler"
	"zoiko.io/compliance-risk-scoring-svc/internal/health"
	"zoiko.io/compliance-risk-scoring-svc/internal/service"
	"zoiko.io/compliance-risk-scoring-svc/internal/store"
)

// policyAuthz is a policy-driven authorization-svc stub: each action maps to
// an outcome. Unlisted actions are DENIED (fail closed), mirroring
// authorization-svc's default-deny.
type policyAuthz struct {
	mu       sync.Mutex
	outcomes map[string]error // action -> nil (GRANTED) | authz.ErrAuthorizationDenied | authz.ErrAuthzServiceUnavailable
	deny     map[string]bool  // principal|action pairs that are explicitly denied
	calls    []string
}

func allowAll() *policyAuthz {
	return &policyAuthz{outcomes: map[string]error{
		service.ActionRiskScoreCreate:          nil,
		service.ActionRiskScoreReview:          nil,
		service.ActionRiskAssessmentArchive:    nil,
		service.ActionRiskThresholdRuleCreate:  nil,
		service.ActionRiskThresholdRuleUpdate:  nil,
		service.ActionRiskThresholdRuleApprove: nil,
		service.ActionRiskThresholdRuleRetire:  nil,
	}, deny: map[string]bool{}}
}

func (p *policyAuthz) CheckAllowed(_ context.Context, tenantID, principalID, legalEntityID, action string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, tenantID+"|"+principalID+"|"+legalEntityID+"|"+action)
	if p.deny[principalID+"|"+action] {
		return authz.ErrAuthorizationDenied
	}
	outcome, listed := p.outcomes[action]
	if !listed {
		return authz.ErrAuthorizationDenied
	}
	return outcome
}

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// setupTestRouter builds a router with no database. It is enough for every
// path refused before the service touches storage (tenant gate, envelope,
// principal, body validation, authorization).
func setupTestRouter() http.Handler {
	return routerWith(nil, allowAll(), fakePinger{})
}

func routerWith(s *store.Store, a service.Authorizer, ready health.Pinger) http.Handler {
	logger := zap.NewNop()
	return handler.NewRouter(handler.NewHandler(service.New(s, a, logger), ready, logger))
}

// testStore connects to TEST_DATABASE_URL (all up migrations applied) or
// skips. Run it as a NOSUPERUSER NOBYPASSRLS role so RLS is exercised.
func testStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres-backed handler test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return store.New(pool), pool
}

// client sends requests as one tenant with sensible envelope defaults.
type client struct {
	t      *testing.T
	router http.Handler
	tenant string
	entity string
}

func newClient(t *testing.T, router http.Handler) *client {
	return &client{t: t, router: router, tenant: "tenant-" + uuid.NewString(), entity: "LE-" + uuid.NewString()[:8]}
}

type reqOpt func(*http.Request)

func as(principal string) reqOpt {
	return func(r *http.Request) { r.Header.Set("X-Principal-Id", principal) }
}
func tenant(id string) reqOpt   { return func(r *http.Request) { r.Header.Set("X-Tenant-Id", id) } }
func idem(key string) reqOpt    { return func(r *http.Request) { r.Header.Set("Idempotency-Key", key) } }
func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func drop(k string) reqOpt      { return func(r *http.Request) { r.Header.Del(k) } }

func (c *client) do(method, path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	c.t.Helper()
	var reader *bytes.Reader
	switch b := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case []byte:
		reader = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", c.tenant)
	req.Header.Set("X-Principal-Id", "principal-1")
	req.Header.Set("X-Legal-Entity-Id", c.entity)
	for _, opt := range opts {
		opt(req)
	}
	withEnvelope(req)
	for _, opt := range opts { // re-apply so drop() wins over withEnvelope defaults
		opt(req)
	}
	rec := httptest.NewRecorder()
	c.router.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %d body %q: %v", rec.Code, rec.Body.String(), err)
	}
	return out
}

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) map[string]any {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("expected %d, got %d: %s", want, rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		return nil
	}
	return decode(t, rec)
}

func expectProblem(t *testing.T, rec *httptest.ResponseRecorder, want int, code string) {
	t.Helper()
	body := expectStatus(t, rec, want)
	if body["code"] != code {
		t.Fatalf("expected problem code %q, got %v (%s)", code, body["code"], rec.Body.String())
	}
}

func counts(o, p, a, d, x int) map[string]any {
	return map[string]any{
		"open_obligations_count": o, "policy_violations_count": p, "audit_exceptions_count": a,
		"privacy_incidents_count": d, "tax_penalties_count": x,
	}
}

func calcBody(entity, name string, c map[string]any) map[string]any {
	body := map[string]any{"legal_entity_id": entity, "assessment_name": name}
	for k, v := range c {
		body[k] = v
	}
	return body
}

var errBoom = errors.New("boom")
