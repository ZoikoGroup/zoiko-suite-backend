package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

type fakeAuthz struct {
	err     error
	asked   []string
	allowed map[string]bool
}

func (f *fakeAuthz) CheckAllowed(_ context.Context, _, _, action, _ string) error {
	f.asked = append(f.asked, action)
	if f.err != nil {
		return f.err
	}
	if f.allowed != nil && !f.allowed[action] {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

// adminRepo answers the connection reads a handler test needs with a configurable result.
type adminRepo struct {
	*memRepo
	getErr  error
	detail  *ConnectionDetail
	decide  func(approve bool, actor, fp string) error
	lastFP  string
	created []Connection
}

func (a *adminRepo) GetConnection(_ context.Context, _, _ string) (*ConnectionDetail, error) {
	return a.detail, a.getErr
}
func (a *adminRepo) ListConnections(context.Context, string) ([]Connection, error) {
	return nil, a.getErr
}
func (a *adminRepo) DecideVersion(_ context.Context, _, _ string, _ int, approve bool, actor, fp, _, _ string) (*ConnectionDetail, error) {
	a.lastFP = fp
	if a.decide != nil {
		if err := a.decide(approve, actor, fp); err != nil {
			return nil, err
		}
	}
	return a.detail, nil
}
func (a *adminRepo) CreateConnection(_ context.Context, c Connection, _ Version, _, _ string) error {
	a.created = append(a.created, c)
	return nil
}

type hrig struct {
	h     *Handler
	r     chi.Router
	authz *fakeAuthz
	repo  *adminRepo
	login *loginRig
}

func newHRig(t *testing.T) *hrig {
	t.Helper()
	lr := newLoginRig(t)
	repo := &adminRepo{memRepo: lr.repo, detail: &ConnectionDetail{Connection: testConnection()}}
	svc := NewService(repo, nil, NewKeyManager(repo, &fakeFetcher{}, repo, nil), nil)
	az := &fakeAuthz{}
	h := NewHandler(svc, lr.login, az, zap.NewNop())
	r := chi.NewRouter()
	h.Register(r)
	return &hrig{h: h, r: r, authz: az, repo: repo, login: lr}
}

func (h *hrig) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	return w
}

var admin = map[string]string{"X-Tenant-Id": tTenant, "X-Principal-Id": "admin-1", "X-Correlation-ID": "corr-1"}

func TestAdminRoutesNeedAnIdentityAndAGrant(t *testing.T) {
	h := newHRig(t)
	routes := []struct{ method, path, action string }{
		{"GET", "/v1/federation/connections", ActionRead},
		{"GET", "/v1/federation/connections/c-1", ActionRead},
		{"POST", "/v1/federation/connections", ActionConfigure},
		{"POST", "/v1/federation/connections/c-1/versions/1/approve", ActionApprove},
		{"POST", "/v1/federation/connections/c-1/versions/1/reject", ActionApprove},
		{"POST", "/v1/federation/connections/c-1/rotate-metadata", ActionConfigure},
		{"POST", "/v1/federation/connections/c-1/keys/refresh", ActionConfigure},
		{"POST", "/v1/federation/connections/c-1/suspend", ActionSuspend},
		{"POST", "/v1/federation/connections/c-1/resume", ActionSuspend},
		{"POST", "/v1/federation/connections/c-1/revoke", ActionSuspend},
		{"GET", "/v1/federation/connections/c-1/logins", ActionRead},
		{"POST", "/v1/federation/links", ActionLink},
		{"GET", "/v1/federation/links", ActionRead},
		{"GET", "/v1/federation/links/l-1", ActionRead},
	}
	for _, rt := range routes {
		// no identity at all
		if w := h.do(rt.method, rt.path, `{}`, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without identity: %d", rt.method, rt.path, w.Code)
		}
		// identity without the grant
		h.authz.allowed = map[string]bool{}
		if w := h.do(rt.method, rt.path, `{}`, admin); w.Code != http.StatusForbidden {
			t.Errorf("%s %s without the grant: %d", rt.method, rt.path, w.Code)
		}
		// authorization unavailable fails closed, as 503, never as success
		h.authz.allowed, h.authz.err = nil, errors.New("down")
		if w := h.do(rt.method, rt.path, `{}`, admin); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s with authz down: %d", rt.method, rt.path, w.Code)
		}
		h.authz.err = nil
		// the right action is the one asked about
		h.authz.asked = nil
		_ = h.do(rt.method, rt.path, `{}`, admin)
		if len(h.authz.asked) != 1 || h.authz.asked[0] != rt.action {
			t.Errorf("%s %s asked %v, want %s", rt.method, rt.path, h.authz.asked, rt.action)
		}
	}
}

func TestErrorsMapToTheRightStatusAndNeverLeakInternals(t *testing.T) {
	h := newHRig(t)
	cases := map[error]int{
		ErrNotFound:         http.StatusNotFound,
		ErrLinkNotFound:     http.StatusNotFound,
		ErrInvalid:          http.StatusBadRequest,
		ErrSelfApproval:     http.StatusForbidden,
		ErrFingerprint:      http.StatusConflict,
		ErrNotPending:       http.StatusConflict,
		ErrTransition:       http.StatusConflict,
		ErrConflict:         http.StatusConflict,
		ErrUnknownPrincipal: http.StatusUnprocessableEntity,
		ErrKeysUnavailable:  http.StatusServiceUnavailable,
	}
	for err, want := range cases {
		h.repo.getErr = fmt.Errorf("wrapped: %w", err)
		if w := h.do("GET", "/v1/federation/connections/c-1", "", admin); w.Code != want {
			t.Errorf("%v: %d, want %d", err, w.Code, want)
		}
	}
	h.repo.getErr = errors.New("pq: password authentication failed for user app_identity (SECRET-DETAIL)")
	w := h.do("GET", "/v1/federation/connections/c-1", "", admin)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "SECRET-DETAIL") {
		t.Fatalf("an internal error is a plain 500: %d %s", w.Code, w.Body.String())
	}
}

func TestApprovalCarriesTheFingerprintTheApproverSaw(t *testing.T) {
	h := newHRig(t)
	w := h.do("POST", "/v1/federation/connections/c-1/versions/2/approve", `{"fingerprint":"abc123","reason":"checked"}`, admin)
	if w.Code != http.StatusOK || h.repo.lastFP != "abc123" {
		t.Fatalf("%d %q", w.Code, h.repo.lastFP)
	}
	if w := h.do("POST", "/v1/federation/connections/c-1/versions/zero/approve", `{}`, admin); w.Code != http.StatusBadRequest {
		t.Fatalf("version: %d", w.Code)
	}
	if w := h.do("POST", "/v1/federation/connections/c-1/versions/0/approve", `{}`, admin); w.Code != http.StatusBadRequest {
		t.Fatalf("version 0: %d", w.Code)
	}
	// a rejection needs a reason
	if w := h.do("POST", "/v1/federation/connections/c-1/versions/2/reject", `{}`, admin); w.Code != http.StatusBadRequest {
		t.Fatalf("reject without reason: %d", w.Code)
	}
	// unknown fields are refused, not ignored
	if w := h.do("POST", "/v1/federation/connections/c-1/versions/2/approve", `{"fingerprint":"x","approve_anyway":true}`, admin); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", w.Code)
	}
}

func TestConfigureReturnsAcceptedAndValidatesInput(t *testing.T) {
	h := newHRig(t)
	good := `{"name":"corp","config":{"issuer":"https://idp.example.com","audience":"zoiko","jwks_uri":"https://idp.example.com/jwks"}}`
	h.repo.detail = &ConnectionDetail{Connection: testConnection()}
	if w := h.do("POST", "/v1/federation/connections", good, admin); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(h.repo.created) != 1 || h.repo.created[0].CreatedBy != "admin-1" {
		t.Fatalf("the author is the verified caller: %+v", h.repo.created)
	}
	for name, body := range map[string]string{
		"weak algorithm": `{"name":"x","config":{"issuer":"https://idp.example.com","audience":"a","jwks_uri":"https://idp.example.com/j","allowed_algs":["HS256"]}}`,
		"http issuer":    `{"name":"x","config":{"issuer":"http://idp.example.com","audience":"a","jwks_uri":"https://idp.example.com/j"}}`,
		"no name":        `{"config":{"issuer":"https://idp.example.com","audience":"a","jwks_uri":"https://idp.example.com/j"}}`,
		"junk":           `not json`,
	} {
		if w := h.do("POST", "/v1/federation/connections", body, admin); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	// the caller cannot name another tenant or author in the body
	if w := h.do("POST", "/v1/federation/connections", `{"tenant_id":"t-2","created_by":"x","name":"n","config":{}}`, admin); w.Code != http.StatusBadRequest {
		t.Fatalf("%d", w.Code)
	}
}

func TestLoginRouteAnswersUniformlyAndUncached(t *testing.T) {
	h := newHRig(t)
	body := func(tok string) string {
		b, _ := json.Marshal(LoginRequest{TenantID: tTenant, IDToken: tok, Nonce: "n-1"})
		return string(b)
	}

	w := h.do("POST", "/v1/authenticate/federated", body(h.login.idp.mint(t, h.login.idp.claims())), nil)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	var resp domain.AuthenticateResponse
	if json.Unmarshal(w.Body.Bytes(), &resp) != nil || resp.AccessToken == "" || resp.PrincipalID != "p-1" {
		t.Fatalf("%s", w.Body.String())
	}

	// every refusal is the identical 401 body
	bodies := map[string]bool{}
	for _, tok := range []string{"garbage", h.login.idp.mint(t, func() map[string]any { c := h.login.idp.claims(); c["aud"] = "x"; return c }())} {
		w := h.do("POST", "/v1/authenticate/federated", body(tok), nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%d", w.Code)
		}
		bodies[w.Body.String()] = true
	}
	if len(bodies) != 1 {
		t.Fatalf("refusals must be indistinguishable on the wire: %v", bodies)
	}
	if w := h.do("POST", "/v1/authenticate/federated", `{"tenant_id":"t"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("%d", w.Code)
	}
	if w := h.do("POST", "/v1/authenticate/federated", `x`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("%d", w.Code)
	}
	h.login.repo.findErr = errBoom
	if w := h.do("POST", "/v1/authenticate/federated", body(h.login.idp.mint(t, h.login.idp.claims())), nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d", w.Code)
	}
	// no administrative header is needed, and none is trusted: the route needs no authz check
	if len(h.authz.asked) != 0 {
		t.Fatalf("login asked authz %v", h.authz.asked)
	}
}

func (a *adminRepo) SetStatus(context.Context, string, string, string, string, string, string) (*Connection, error) {
	return nil, ErrNotFound
}
func (a *adminRepo) AddVersion(context.Context, string, string, Version, string, string) (*Version, error) {
	return nil, ErrNotFound
}
func (a *adminRepo) GetLink(context.Context, string, string) (*Link, error) {
	return nil, ErrLinkNotFound
}
func (a *adminRepo) ListLinks(context.Context, string, string, string, int) ([]Link, error) {
	return nil, nil
}
func (a *adminRepo) CreateLink(context.Context, Link, string, string) (*Link, error) {
	return nil, ErrNotFound
}
func (a *adminRepo) ListLogins(context.Context, string, string, int) ([]LoginRecord, error) {
	return nil, nil
}
