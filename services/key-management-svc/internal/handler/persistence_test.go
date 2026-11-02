package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/key-management-svc/internal/authz"
	"zoiko.io/key-management-svc/internal/domain"
	"zoiko.io/key-management-svc/internal/handler"
	"zoiko.io/key-management-svc/internal/idempotency"
	"zoiko.io/key-management-svc/internal/siem"
	"zoiko.io/key-management-svc/internal/store"
	"zoiko.io/key-management-svc/internal/testdb"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func newHandler(s store.Store, az handler.AuthzChecker) *handler.Handler {
	return handler.New(s, siem.New("", "key-management-svc", zap.NewNop()), az, zap.NewNop())
}

// /healthz and /readyz answer with NO identity headers, even in strict
// envelope mode, and /readyz reflects the database.
func TestProbesArePublicAndReadyzReflectsDB(t *testing.T) {
	t.Setenv("ZS_ENVELOPE_ENFORCEMENT", "strict")
	probe := func(h *handler.Handler, path string) int {
		w := httptest.NewRecorder()
		handler.NewRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code
	}
	base := func() *handler.Handler { return newHandler(store.NewMemoryStore(), &stubAuthz{}) }
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

func call(r http.Handler, method, path, tenant, principal, idemKey string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("X-Principal-Id", principal)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	withEnvelope(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func registerBody(le string) []byte {
	b, _ := json.Marshal(map[string]string{
		"legal_entity_id": le, "key_alias": "k", "key_model": "BYOK", "key_provider": "AWS_KMS",
		"external_key_arn": "arn:aws:kms:eu-west-2:111122223333:key/" + uuid.NewString(),
	})
	return b
}

func decodeKey(t *testing.T, w *httptest.ResponseRecorder) domain.CustomerKey {
	t.Helper()
	var k domain.CustomerKey
	if err := json.Unmarshal(w.Body.Bytes(), &k); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return k
}

// A retried rotate carrying the same Idempotency-Key is answered from the
// first response: the key advances ONE version, not two.
func TestRotate_DuplicateRequestIsReplayed(t *testing.T) {
	r := handler.NewRouter(newHandler(store.NewMemoryStore(), &stubAuthz{}).WithIdempotency(idempotency.NewMemoryStore()))
	reg := call(r, http.MethodPost, "/v1/keys", "t1", "p1", "", registerBody("le-1"))
	if reg.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", reg.Code, reg.Body.String())
	}
	id := decodeKey(t, reg).ID

	first := call(r, http.MethodPost, "/v1/keys/"+id+"/rotate", "t1", "p1", "rot-1", nil)
	second := call(r, http.MethodPost, "/v1/keys/"+id+"/rotate", "t1", "p1", "rot-1", nil)
	if first.Code != 200 || second.Code != 200 || second.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("codes %d/%d, replay header %q", first.Code, second.Code, second.Header().Get("X-Idempotent-Replay"))
	}
	got := decodeKey(t, call(r, http.MethodGet, "/v1/keys/"+id, "t1", "p1", "", nil))
	if got.KeyVersion != 2 {
		t.Fatalf("version after duplicate rotate = %d, want 2", got.KeyVersion)
	}
}

// policyAuthz grants exactly the (principal, legal entity, action) triples it
// holds and denies everything else, like authorization-svc.
type policyAuthz map[[3]string]bool

func (p policyAuthz) CheckAllowed(_ context.Context, principal, le, action string) error {
	if p[[3]string{principal, le, action}] {
		return nil
	}
	return authz.ErrAuthorizationDenied
}

// One router, one policy. ops may register, rotate and read in le-1 but not
// disable; the disable is 403 and the key stays ENABLED.
func TestAuthz_PolicyDrivenAllowAndDeny(t *testing.T) {
	az := policyAuthz{
		{"ops", "le-1", handler.KeyRegister}: true,
		{"ops", "le-1", handler.KeyRead}:     true,
		{"ops", "le-1", handler.KeyRotate}:   true,
	}
	r := handler.NewRouter(newHandler(store.NewMemoryStore(), az))
	reg := call(r, http.MethodPost, "/v1/keys", "t1", "ops", "", registerBody("le-1"))
	if reg.Code != http.StatusCreated {
		t.Fatalf("allowed register = %d", reg.Code)
	}
	id := decodeKey(t, reg).ID
	for _, tc := range []struct {
		name, method, path, principal string
		body                          []byte
		want                          int
	}{
		{"register other entity", http.MethodPost, "/v1/keys", "ops", registerBody("le-2"), http.StatusForbidden},
		{"rotate allowed", http.MethodPost, "/v1/keys/" + id + "/rotate", "ops", nil, http.StatusOK},
		{"rotate other principal", http.MethodPost, "/v1/keys/" + id + "/rotate", "mallory", nil, http.StatusForbidden},
		{"disable not granted", http.MethodPost, "/v1/keys/" + id + "/disable", "ops", nil, http.StatusForbidden},
		{"read allowed", http.MethodGet, "/v1/keys/" + id, "ops", nil, http.StatusOK},
	} {
		if w := call(r, tc.method, tc.path, "t1", tc.principal, "", tc.body); w.Code != tc.want {
			t.Errorf("%s = %d, want %d: %s", tc.name, w.Code, tc.want, w.Body.String())
		}
	}
	if got := decodeKey(t, call(r, http.MethodGet, "/v1/keys/"+id, "t1", "ops", "", nil)); got.State != domain.StateEnabled {
		t.Fatalf("denied disable changed state to %s", got.State)
	}
}

// End to end on Postgres as the ordinary app role: register + rotate +
// disable through one router; a router on a NEW pool (restart) serves the
// same lifecycle; another tenant gets 404 on read/rotate/disable; a duplicate
// rotate is replayed from the Postgres idempotency store; /readyz follows the
// pool. No response body contains anything but the external key reference.
func TestPostgres_EndToEnd(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	ctx := context.Background()
	tenant, other := "t-"+uuid.NewString(), "t-"+uuid.NewString()
	build := func(pool *pgxpool.Pool) http.Handler {
		return handler.NewRouter(newHandler(store.NewPgStore(pool), &stubAuthz{}).
			WithIdempotency(idempotency.NewPgStore(pool)).
			WithReadiness(pool))
	}
	r := build(app)
	reg := call(r, http.MethodPost, "/v1/keys", tenant, "p1", "", registerBody("le-1"))
	if reg.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", reg.Code, reg.Body.String())
	}
	id := decodeKey(t, reg).ID
	rotKey := "rot-" + uuid.NewString()
	if w := call(r, http.MethodPost, "/v1/keys/"+id+"/rotate", tenant, "p1", rotKey, nil); w.Code != 200 {
		t.Fatalf("rotate = %d: %s", w.Code, w.Body.String())
	}

	fresh, err := pgxpool.New(ctx, pools.AppDSN)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	r2 := build(fresh)
	// Same Idempotency-Key after the "restart": replayed, not re-run.
	dup := call(r2, http.MethodPost, "/v1/keys/"+id+"/rotate", tenant, "p1", rotKey, nil)
	if dup.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("duplicate rotate after restart was not replayed: %d %s", dup.Code, dup.Body.String())
	}
	got := decodeKey(t, call(r2, http.MethodGet, "/v1/keys/"+id, tenant, "p1", "", nil))
	if got.KeyVersion != 2 || got.RotationCount != 1 {
		t.Fatalf("after restart: %+v", got)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/keys/" + id},
		{http.MethodPost, "/v1/keys/" + id + "/rotate"},
		{http.MethodPost, "/v1/keys/" + id + "/disable"},
	} {
		if w := call(r2, tc.method, tc.path, other, "p1", "", nil); w.Code != http.StatusNotFound {
			t.Errorf("other tenant %s %s = %d, want 404", tc.method, tc.path, w.Code)
		}
	}
	if w := call(r2, http.MethodPost, "/v1/keys/"+id+"/disable", tenant, "p1", "", nil); w.Code != 200 {
		t.Fatalf("disable = %d", w.Code)
	}
	body := call(r2, http.MethodGet, "/v1/keys/"+id, tenant, "p1", "", nil).Body.String()
	if !strings.Contains(body, `"state":"DISABLED"`) {
		t.Fatalf("disable not persisted: %s", body)
	}
	for _, forbidden := range []string{"material", "plaintext", "private_key", "secret"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("response mentions %q: %s", forbidden, body)
		}
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
