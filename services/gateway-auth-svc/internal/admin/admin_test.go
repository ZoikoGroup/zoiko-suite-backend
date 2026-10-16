package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zoiko.io/gateway-auth-svc/internal/admission"
)

const token = "0123456789abcdef0123456789abcdef" // gitleaks:allow -- throwaway test token for an in-memory test server, not a credential

func fixture(t *testing.T) (http.Handler, *admission.Recorder, *admission.Limiter) {
	t.Helper()
	rec := admission.NewRecorder(32, 0, nil)
	lim := admission.NewLimiter(1, 5, 0, nil)
	h, err := New(Source{
		Recorder: rec, IPLimiter: lim, TenantLimiter: admission.NewLimiter(0, 1, 0, nil),
		Policy: admission.Policy{MaxBodyBytes: 1000, SupportedVersions: []string{"v1"}},
	}, token)
	if err != nil {
		t.Fatal(err)
	}
	return h, rec, lim
}

func get(h http.Handler, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEveryRouteRequiresTheToken(t *testing.T) {
	h, _, _ := fixture(t)
	for _, p := range []string{
		"/internal/v1/gateway/decisions/x", "/internal/v1/gateway/denied", "/internal/v1/gateway/usage/t",
		"/internal/v1/gateway/rate-limit/client/k", "/internal/v1/gateway/routes/versions", "/unknown",
	} {
		for name, auth := range map[string]string{"none": "", "wrong": "Bearer nope", "basic": "Basic " + token, "longer": "Bearer " + token + "x"} {
			rec := get(h, p, auth)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s with %s credential: status %d, want 401", p, name, rec.Code)
			}
			if rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("%s: missing WWW-Authenticate", p)
			}
			if strings.Contains(rec.Body.String(), token) {
				t.Errorf("%s: the response leaks the token", p)
			}
		}
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	h, _, _ := fixture(t)
	if rec := get(h, "/internal/v1/gateway/routes/versions", "bearer "+token); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestDecisionLookupAndList(t *testing.T) {
	h, rec, _ := fixture(t)
	d, _ := rec.Record(admission.Decision{TenantID: "t1", Outcome: "no_token", Code: "INT-AUTH-003", Status: 401})
	rec.Record(admission.Decision{TenantID: "t2", Outcome: "rate_limited", Code: "INT-RATE-007", Status: 429})

	r := get(h, "/internal/v1/gateway/decisions/"+d.ID, "Bearer "+token)
	if r.Code != 200 {
		t.Fatalf("status %d", r.Code)
	}
	var got admission.Decision
	_ = json.Unmarshal(r.Body.Bytes(), &got)
	if got.ID != d.ID || got.Outcome != "no_token" {
		t.Fatalf("%+v", got)
	}
	if get(h, "/internal/v1/gateway/decisions/nope", "Bearer "+token).Code != 404 {
		t.Fatal("unknown decision must be 404")
	}

	var list struct {
		Decisions []admission.Decision `json:"decisions"`
	}
	r = get(h, "/internal/v1/gateway/denied?tenant_id=t1", "Bearer "+token)
	_ = json.Unmarshal(r.Body.Bytes(), &list)
	if len(list.Decisions) != 1 || list.Decisions[0].TenantID != "t1" {
		t.Fatalf("tenant filter: %+v", list.Decisions)
	}
	r = get(h, "/internal/v1/gateway/denied?tenant_id=nobody", "Bearer "+token)
	if !strings.Contains(r.Body.String(), `"decisions":[]`) {
		t.Fatalf("an empty result must be [], not null: %s", r.Body.String())
	}
	for _, bad := range []string{"0", "-1", "abc", "1001"} {
		if get(h, "/internal/v1/gateway/denied?limit="+bad, "Bearer "+token).Code != 400 {
			t.Errorf("limit=%s accepted", bad)
		}
	}
}

func TestUsageAndRateLimitState(t *testing.T) {
	h, rec, lim := fixture(t)
	rec.Allowed("t1")
	rec.Allowed("t1")
	rec.Record(admission.Decision{TenantID: "t1", Outcome: "rate_limited"})
	r := get(h, "/internal/v1/gateway/usage/t1", "Bearer "+token)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"allowed":2`) || !strings.Contains(r.Body.String(), `"rate_limited":1`) {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	if get(h, "/internal/v1/gateway/usage/nobody", "Bearer "+token).Code != 404 {
		t.Fatal("unseen tenant must be 404")
	}

	lim.Allow("k1")
	r = get(h, "/internal/v1/gateway/rate-limit/client/k1", "Bearer "+token)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"enabled":true`) || !strings.Contains(r.Body.String(), `"burst":5`) {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	// reading state spends nothing
	before := get(h, "/internal/v1/gateway/rate-limit/client/k1", "Bearer "+token).Body.String()
	after := get(h, "/internal/v1/gateway/rate-limit/client/k1", "Bearer "+token).Body.String()
	if !strings.Contains(before, `"remaining_tokens":4`) || !strings.Contains(after, `"remaining_tokens":4`) {
		t.Fatalf("state must be read-only: %s / %s", before, after)
	}
	if r = get(h, "/internal/v1/gateway/rate-limit/tenant/t1", "Bearer "+token); !strings.Contains(r.Body.String(), `"enabled":false`) {
		t.Fatalf("a disabled limiter reports so: %s", r.Body.String())
	}
	if get(h, "/internal/v1/gateway/rate-limit/galaxy/k", "Bearer "+token).Code != 400 {
		t.Fatal("unknown scope must be 400")
	}
}

func TestRouteVersionStatus(t *testing.T) {
	h, _, _ := fixture(t)
	r := get(h, "/internal/v1/gateway/routes/versions", "Bearer "+token)
	for _, want := range []string{`"supported_versions":["v1"]`, `"version_admission":true`, `"max_body_bytes":1000`, `"compressed_bodies":false`} {
		if !strings.Contains(r.Body.String(), want) {
			t.Errorf("missing %s in %s", want, r.Body.String())
		}
	}
}

func TestReadOnlyAndNoStore(t *testing.T) {
	h, _, _ := fixture(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(m, "/internal/v1/gateway/denied", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Errorf("%s answered %d: the query surface must be read-only", m, rec.Code)
		}
	}
	if cc := get(h, "/internal/v1/gateway/routes/versions", "Bearer "+token).Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
}

func TestNewRefusesWeakOrIncompleteSetup(t *testing.T) {
	if _, err := New(Source{Recorder: admission.NewRecorder(1, 0, nil)}, "short"); err == nil {
		t.Error("a short token was accepted")
	}
	if _, err := New(Source{}, token); err == nil {
		t.Error("a missing recorder was accepted")
	}
}

func TestLoadToken(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "t")
	_ = os.WriteFile(good, []byte("  "+token+"\r\n"), 0o600)
	if got, err := LoadToken(good); err != nil || got != token {
		t.Fatalf("%q %v", got, err)
	}
	short := filepath.Join(dir, "s")
	_ = os.WriteFile(short, []byte("tiny"), 0o600)
	if _, err := LoadToken(short); err == nil {
		t.Error("a short token file was accepted")
	}
	if _, err := LoadToken(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing token file was accepted")
	}
}
