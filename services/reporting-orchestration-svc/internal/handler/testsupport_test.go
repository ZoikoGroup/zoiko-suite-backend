package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/reporting-orchestration-svc/internal/archivestore"
	"zoiko.io/reporting-orchestration-svc/internal/authz"
	"zoiko.io/reporting-orchestration-svc/internal/handler"
	"zoiko.io/reporting-orchestration-svc/internal/store"
)

// policyAuthz is a policy-driven authorization-svc stub. Every action is
// GRANTED unless denied for a principal or overridden for an action.
type policyAuthz struct {
	mu       sync.Mutex
	override map[string]error // action -> outcome
	deny     map[string]bool  // principal|action
	calls    []string
}

func newPolicyAuthz() *policyAuthz {
	return &policyAuthz{override: map[string]error{}, deny: map[string]bool{}}
}

func (p *policyAuthz) CheckAllowed(_ context.Context, tenantID, principalID, legalEntityID, action string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, tenantID+"|"+principalID+"|"+legalEntityID+"|"+action)
	if p.deny[principalID+"|"+action] {
		return authz.ErrAuthorizationDenied
	}
	if outcome, ok := p.override[action]; ok {
		return outcome
	}
	return nil
}

// fakeArchives mimics audit-event-store-svc: ids containing "missing" fail,
// ids containing "baddigest" return a non-hex digest, others a real digest.
type fakeArchives struct{}

func (fakeArchives) GetArchive(_ context.Context, archiveID, _, _ string) (*archivestore.Archive, error) {
	switch {
	case strings.Contains(archiveID, "missing"):
		return nil, archivestore.ErrArchiveServiceUnavailable
	case strings.Contains(archiveID, "baddigest"):
		return &archivestore.Archive{ArchiveID: archiveID, ArchiveDigest: "not-a-digest"}, nil
	}
	return &archivestore.Archive{ArchiveID: archiveID, EventCount: 3,
		ArchiveDigest: "a3f5b1c2d4e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f80"}, nil
}

// fakeRetention: entity refs containing "hold" are under legal hold.
type fakeRetention struct{}

func (fakeRetention) IsBlocked(_ context.Context, _, _, entityRef, _, _ string) (bool, error) {
	return strings.Contains(entityRef, "hold"), nil
}

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

var errBoom = errors.New("boom")

// newRouterNoDB serves every path that is refused before storage (tenant
// gate, envelope, probes). Store dependencies are nil and never reached.
func newRouterNoDB(t *testing.T) http.Handler {
	t.Helper()
	return handler.NewRouter(handler.NewHandler(handler.Dependencies{
		Archives: fakeArchives{}, Retentions: fakeRetention{}, Authz: newPolicyAuthz(),
		Ready: fakePinger{}, Logger: zap.NewNop(),
	}))
}

// newRouter is backed by the Postgres named by TEST_DATABASE_URL (all up
// migrations applied); it skips when unset.
func newRouter(t *testing.T) http.Handler {
	t.Helper()
	router, _ := newRouterWith(t, newPolicyAuthz())
	return router
}

func newRouterWith(t *testing.T, a handler.Authorizer) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping Postgres-backed handler test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	s := store.NewPgStore(pool)
	return handler.NewRouter(handler.NewHandler(handler.Dependencies{
		Definitions: s, Runs: s, Exports: s, Idempotency: s,
		Archives: fakeArchives{}, Retentions: fakeRetention{}, Authz: a,
		Ready: pool, Logger: zap.NewNop(),
	})), pool
}

// client sends requests as one (unique) tenant and legal entity.
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
	var raw []byte
	switch b := body.(type) {
	case nil:
	case []byte:
		raw = b
	default:
		var err error
		if raw, err = json.Marshal(b); err != nil {
			c.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", c.tenant)
	req.Header.Set("X-Principal-Id", "principal-01")
	req.Header.Set("X-Legal-Entity-Id", c.entity)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	for _, opt := range opts {
		opt(req)
	}
	withEnvelope(req)
	for _, opt := range opts { // so drop() also beats withEnvelope's defaults
		opt(req)
	}
	rec := httptest.NewRecorder()
	c.router.ServeHTTP(rec, req)
	return rec
}

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) map[string]any {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("expected %d, got %d: %s", want, rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func expectProblem(t *testing.T, rec *httptest.ResponseRecorder, want int, code string) {
	t.Helper()
	body := expectStatus(t, rec, want)
	if body["code"] != code {
		t.Fatalf("expected problem code %q, got %v: %s", code, body["code"], rec.Body.String())
	}
}

func (c *client) definitionBody(reportType string) map[string]any {
	return map[string]any{"legal_entity_id": c.entity, "report_name": "Report " + uuid.NewString()[:6],
		"report_type": reportType, "output_format": "JSON", "data_sources": []string{"ledger-svc", "treasury-svc"}}
}

// activeDefinition creates a definition and walks it DRAFT -> VALIDATED ->
// ACTIVE (REP-05), returning its id.
func (c *client) activeDefinition(reportType string) string {
	c.t.Helper()
	def := expectStatus(c.t, c.do(http.MethodPost, "/v1/reports/definitions", c.definitionBody(reportType)), http.StatusCreated)
	id := def["id"].(string)
	expectStatus(c.t, c.do(http.MethodPatch, "/v1/reports/definitions/"+id+"/status", map[string]string{"status": "VALIDATED"}), http.StatusOK)
	expectStatus(c.t, c.do(http.MethodPatch, "/v1/reports/definitions/"+id+"/status", map[string]string{"status": "ACTIVE"}), http.StatusOK)
	return id
}
