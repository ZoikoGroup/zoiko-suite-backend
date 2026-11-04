package context_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/idempotency"
	"zoiko.io/identity-context-svc/internal/lifecycle"
	"zoiko.io/identity-context-svc/internal/ratelimit"
	"zoiko.io/identity-context-svc/internal/store"
)

// TestAU09_WorkloadCreatedActiveWithoutPassword pins what reaches the store,
// not what a mock echoes back: a workload is created ACTIVE (it never
// activates through a link) and a person PENDING_ACTIVATION.
func TestAU09_WorkloadCreatedActiveWithoutPassword(t *testing.T) {
	st := &mockLifecycleStore{detail: &store.PrincipalDetail{}}
	r := lifecycleRouter(newLifecycleService(st), &permittingAuthz{})

	w := postLifecycle(t, r, "/v1/principals", "tenant-a", "admin-1",
		map[string]any{"display_name": "Nightly batch", "principal_type": "SERVICE_ACCOUNT"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	w = postLifecycle(t, r, "/v1/principals", "tenant-a", "admin-1",
		map[string]any{"email": "a@zoiko.io", "display_name": "A Person", "principal_type": "HUMAN", "invite": false})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	require.Len(t, st.created, 2)
	assert.Equal(t, domain.PrincipalTypeServiceAccount, st.created[0].PrincipalType)
	assert.Equal(t, domain.PrincipalStatusActive, st.created[0].Status, "a workload starts ACTIVE")
	assert.Equal(t, domain.PrincipalStatusPendingActivation, st.created[1].Status, "a person starts PENDING_ACTIVATION")
}

// memIdempotency is an in-memory idempotency.Store.
type memIdempotency struct {
	mu   sync.Mutex
	recs map[string]*store.IdempotencyRecord
}

func (m *memIdempotency) ClaimIdempotencyKey(_ context.Context, tenantID, endpoint, key, fingerprint string) (*store.IdempotencyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := tenantID + "|" + endpoint + "|" + key
	if rec, ok := m.recs[k]; ok {
		return rec, nil
	}
	m.recs[k] = &store.IdempotencyRecord{RequestFingerprint: fingerprint}
	return nil, nil
}
func (m *memIdempotency) CompleteIdempotencyKey(_ context.Context, tenantID, endpoint, key string, status int, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.recs[tenantID+"|"+endpoint+"|"+key]
	rec.ResponseStatus, rec.ResponseBody = status, body
	return nil
}
func (m *memIdempotency) ReleaseIdempotencyKey(_ context.Context, tenantID, endpoint, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, tenantID+"|"+endpoint+"|"+key)
	return nil
}

func TestAU09_CreateSubjectIdempotentReplay(t *testing.T) {
	st := &mockLifecycleStore{detail: &store.PrincipalDetail{Principal: domain.Principal{PrincipalID: "p-new", Status: domain.PrincipalStatusActive}}}
	h := identityctx.NewHandler(nil, nil, nil, nil, &permittingAuthz{}, zap.NewNop()).WithLifecycle(newLifecycleService(st))
	r := chi.NewRouter()
	r.Use(idempotency.Middleware(&memIdempotency{recs: map[string]*store.IdempotencyRecord{}}, zap.NewNop()))
	identityctx.RegisterRoutes(r, h)

	send := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"display_name": "Batch", "principal_type": "SERVICE_ACCOUNT"})
		req := httptest.NewRequest(http.MethodPost, "/v1/principals", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant-Id", "tenant-a")
		req.Header.Set("X-Principal-Id", "admin-1")
		req.Header.Set("Idempotency-Key", "create-batch-1")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	first, second := send(), send()

	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	assert.Equal(t, first.Code, second.Code)
	assert.JSONEq(t, first.Body.String(), second.Body.String(), "a replay answers with the first response")
	assert.Len(t, st.created, 1, "the subject is created once")
}

// TestAU09_PreSessionRoutesThrottled: activation, completing a reset and
// forgot-password are reachable without a session, so they are throttled per
// client address and answer 429 with Retry-After.
func TestAU09_PreSessionRoutesThrottled(t *testing.T) {
	for _, path := range []string{"/v1/account/activate", "/v1/account/password/reset", "/v1/account/password/forgot"} {
		t.Run(path, func(t *testing.T) {
			lim := ratelimit.New(ratelimit.NewMemoryCounter(), ratelimit.Limits{AttemptsPerIPPerMinute: 2,
				FailuresPerIP: 100, FailuresPerTenant: 100, FailureWindow: time.Minute}, nil)
			st := &mockLifecycleStore{consumeErr: domain.ErrCredentialTokenUsed}
			h := identityctx.NewHandler(nil, nil, nil, nil, &permittingAuthz{}, zap.NewNop()).
				WithLifecycle(newLifecycleService(st)).WithLoginThrottle(lim, nil)
			r := chi.NewRouter()
			identityctx.RegisterRoutes(r, h)

			var last *httptest.ResponseRecorder
			for i := 0; i < 3; i++ {
				last = postLifecycle(t, r, path, "tenant-a", "", map[string]any{"token": "t", "password": "Secret12345678", "email": "a@zoiko.io"}) // gitleaks:allow -- test fixture password, not a secret
			}
			assert.Equal(t, http.StatusTooManyRequests, last.Code, last.Body.String())
			assert.NotEmpty(t, last.Header().Get("Retry-After"))
		})
	}
}

// TestAU09_ForgotPasswordBudgetKeyedOnRealClientIP: a client cannot reset its
// forgot-password budget by writing a new X-Forwarded-For on each request.
func TestAU09_ForgotPasswordBudgetKeyedOnRealClientIP(t *testing.T) {
	var keys []string
	budget := func(_ context.Context, name, key string, _ int, _ time.Duration) bool {
		if name == "forgot" {
			keys = append(keys, key)
		}
		return true
	}
	ls := lifecycle.New(&mockLifecycleStore{}, nil, lifecycle.Policy{MinLength: 12}, nil, nil,
		lifecycle.Config{ForgotPerIPPerHr: 5}, budget, zap.NewNop())
	_, proxy, err := net.ParseCIDR("10.0.0.0/8")
	require.NoError(t, err)
	h := identityctx.NewHandler(nil, nil, nil, nil, &permittingAuthz{}, zap.NewNop()).
		WithLifecycle(ls).WithLoginThrottle(nil, []*net.IPNet{proxy})
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)

	send := func(remote, xff string) {
		body, _ := json.Marshal(map[string]any{"email": "a@zoiko.io"})
		req := httptest.NewRequest(http.MethodPost, "/v1/account/password/forgot", bytes.NewReader(body))
		req.Header.Set("X-Tenant-Id", "tenant-a")
		req.Header.Set("X-Forwarded-For", xff)
		req.RemoteAddr = remote
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
	send("203.0.113.9:4000", "6.6.6.6")            // untrusted peer: its XFF is ignored
	send("203.0.113.9:4000", "7.7.7.7")            // a new forged XFF changes nothing
	send("10.1.2.3:4000", "6.6.6.6, 198.51.100.7") // trusted proxy: right-most untrusted hop

	assert.Equal(t, []string{"203.0.113.9", "203.0.113.9", "198.51.100.7"}, keys)
}
