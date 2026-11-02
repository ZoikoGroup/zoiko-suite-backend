package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/carta-svc/internal/authz"
	"zoiko.io/carta-svc/internal/handler"
	"zoiko.io/carta-svc/internal/idempotency"
	"zoiko.io/carta-svc/internal/store"
	"zoiko.io/carta-svc/internal/testdb"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// /healthz and /readyz answer with NO identity headers, even in strict
// envelope mode, and /readyz reflects the database.
func TestProbesArePublicAndReadyzReflectsDB(t *testing.T) {
	t.Setenv("ZS_ENVELOPE_ENFORCEMENT", "strict")
	probe := func(h *handler.Handler, path string) int {
		w := httptest.NewRecorder()
		handler.NewRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code
	}
	base := func() *handler.Handler { return handler.New(store.NewMemoryStore(), &stubAuthz{}, zap.NewNop()) }

	if c := probe(base(), "/healthz"); c != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", c)
	}
	if c := probe(base(), "/readyz"); c != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with no database = %d, want 503", c)
	}
	if c := probe(base().WithReadiness(fakePinger{errors.New("down")}), "/readyz"); c != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with database down = %d, want 503", c)
	}
	if c := probe(base().WithReadiness(fakePinger{}), "/readyz"); c != http.StatusOK {
		t.Fatalf("/readyz with database up = %d, want 200", c)
	}
}

func postEvaluate(r http.Handler, tenantID, idemKey, subject string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{
		"legal_entity_id": "le-1",
		"context": map[string]any{
			"subject_id": subject, "subject_type": "USER", "device_trust_level": 90,
			"ip_address": "10.0.0.1", "is_known_location": true, "resource_sensitivity": "LOW",
			"action_requested": "READ", "time_of_day_hour": 12,
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/carta/evaluate", bytes.NewReader(body))
	req.Header.Set("X-Tenant-ID", tenantID)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func listAs(r http.Handler, tenantID string) []map[string]any {
	req := httptest.NewRequest(http.MethodGet, "/v1/carta/assessments?legal_entity_id=le-1", nil)
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Principal-Id", "reader")
	withEnvelope(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out.Data
}

// A retried /evaluate carrying the same Idempotency-Key is answered from the
// first response and records ONE assessment, not two.
func TestEvaluate_DuplicateRequestIsReplayed(t *testing.T) {
	r := handler.NewRouter(handler.New(store.NewMemoryStore(), &stubAuthz{}, zap.NewNop()).
		WithIdempotency(idempotency.NewMemoryStore()))

	first := postEvaluate(r, "t1", "idem-dup", "alice")
	second := postEvaluate(r, "t1", "idem-dup", "alice")
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("codes = %d, %d; want 201, 201", first.Code, second.Code)
	}
	if second.Header().Get("X-Idempotent-Replay") != "true" || second.Body.String() != first.Body.String() {
		t.Fatalf("second call was not a replay: %q vs %q", second.Body.String(), first.Body.String())
	}
	if n := len(listAs(r, "t1")); n != 1 {
		t.Fatalf("assessments recorded = %d, want 1", n)
	}
}

// policyAuthz grants exactly the (principal, legal entity, action) triples
// it holds and denies everything else, like authorization-svc.
type policyAuthz map[[3]string]bool

func (p policyAuthz) CheckAllowed(_ context.Context, principal, le, action string) error {
	if p[[3]string{principal, le, action}] {
		return nil
	}
	return authz.ErrAuthorizationDenied
}

// One router, one policy: the granted principal is allowed, a different
// principal and a different legal entity are both denied 403.
func TestAuthz_PolicyDrivenAllowAndDeny(t *testing.T) {
	az := policyAuthz{{"alice", "le-1", handler.CartaAssessmentRead}: true}
	r := handler.NewRouter(handler.New(store.NewMemoryStore(), az, zap.NewNop()))
	if w := postEvaluate(r, "t1", "", "subject"); w.Code != http.StatusCreated {
		t.Fatalf("evaluate = %d", w.Code)
	}
	for _, tc := range []struct {
		principal, le string
		want          int
	}{
		{"alice", "le-1", http.StatusOK},
		{"mallory", "le-1", http.StatusForbidden},
		{"alice", "le-2", http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/carta/assessments?legal_entity_id="+tc.le, nil)
		req.Header.Set("X-Tenant-ID", "t1")
		req.Header.Set("X-Principal-Id", tc.principal)
		withEnvelope(req)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%s on %s = %d, want %d: %s", tc.principal, tc.le, w.Code, tc.want, w.Body.String())
		}
	}
}

// End to end on Postgres as the ordinary app role: an assessment written
// through one router is served by a router built on a NEW pool (restart),
// is 404 to another tenant, and a duplicate POST is replayed from the
// Postgres idempotency store. /readyz goes 503 once the pool is closed.
func TestPostgres_EndToEnd(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	ctx := context.Background()
	tenant, other := "t-"+uuid.NewString(), "t-"+uuid.NewString()

	build := func(pool *pgxpool.Pool) http.Handler {
		return handler.NewRouter(handler.New(store.NewPgStore(pool), &stubAuthz{}, zap.NewNop()).
			WithIdempotency(idempotency.NewPgStore(pool)).
			WithReadiness(pool))
	}
	r := build(app)
	first := postEvaluate(r, tenant, "idem-"+uuid.NewString(), "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("evaluate = %d: %s", first.Code, first.Body.String())
	}
	var asm struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &asm)

	fresh, err := pgxpool.New(ctx, pools.AppDSN)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	r2 := build(fresh)
	get := func(r http.Handler, tenantID string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/carta/assessments/"+asm.ID, nil)
		req.Header.Set("X-Tenant-ID", tenantID)
		req.Header.Set("X-Principal-Id", "reader")
		withEnvelope(req)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if c := get(r2, tenant); c != http.StatusOK {
		t.Fatalf("GET after restart = %d, want 200", c)
	}
	if c := get(r2, other); c != http.StatusNotFound {
		t.Fatalf("GET from another tenant = %d, want 404", c)
	}

	key := "idem-" + uuid.NewString()
	a := postEvaluate(r2, tenant, key, "bob")
	b := postEvaluate(r2, tenant, key, "bob")
	if b.Header().Get("X-Idempotent-Replay") != "true" || a.Body.String() != b.Body.String() {
		t.Fatalf("duplicate POST not replayed from Postgres: %d %q / %d %q", a.Code, a.Body.String(), b.Code, b.Body.String())
	}
	if n := len(listAs(r2, tenant)); n != 2 {
		t.Fatalf("assessments = %d, want 2 (alice + one bob)", n)
	}

	w := httptest.NewRecorder()
	r2.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/readyz with live pool = %d", w.Code)
	}
	fresh.Close()
	w = httptest.NewRecorder()
	r2.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with closed pool = %d, want 503", w.Code)
	}
}
