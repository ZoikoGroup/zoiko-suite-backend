package vault

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const ref = "0a000000-0000-4000-8000-000000000001"

var caller = Caller{PrincipalID: "approver-1", TenantID: "tenant-a", CorrelationID: "corr-1"}

func server(t *testing.T, status int, body string, seen *http.Request) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = *r.Clone(context.Background())
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVerifyAcceptsAnActiveVersion(t *testing.T) {
	var seen http.Request
	c := server(t, 200, `[{"version_status":"SUPERSEDED","tenant_id":null},{"version_status":"ACTIVE","tenant_id":"tenant-a"}]`, &seen)
	if err := c.Verify(context.Background(), ref, caller); err != nil {
		t.Fatal(err)
	}
	if seen.URL.Path != "/v1/secret-policies/"+ref+"/versions" {
		t.Fatalf("path %s", seen.URL.Path)
	}
	if seen.Header.Get("X-Principal-Id") != "approver-1" || seen.Header.Get("X-Tenant-Id") != "tenant-a" ||
		seen.Header.Get("X-Correlation-Id") != "corr-1" {
		t.Fatalf("the vault must authorize the person doing the approving: %v", seen.Header)
	}
}

func TestVerifyAcceptsAGlobalPolicy(t *testing.T) {
	c := server(t, 200, `[{"version_status":"ACTIVE","tenant_id":null}]`, nil)
	if err := c.Verify(context.Background(), ref, caller); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"unknown policy":           {404, `{"error":"secret_policy_not_found"}`, ErrNotFound},
		"only drafts":              {200, `[{"version_status":"DRAFT","tenant_id":null}]`, ErrNotActive},
		"only retired":             {200, `[{"version_status":"RETIRED","tenant_id":null}]`, ErrNotActive},
		"another tenant's version": {200, `[{"version_status":"ACTIVE","tenant_id":"tenant-b"}]`, ErrNotActive},
		"no versions":              {200, `[]`, ErrNotActive},
		"unauthenticated":          {401, ``, ErrForbidden},
		"forbidden":                {403, ``, ErrForbidden},
		"vault down":               {503, ``, ErrUnavailable},
		"vault error":              {500, ``, ErrUnavailable},
		"malformed body":           {200, `not json`, ErrUnavailable},
		"unexpected shape":         {200, `{"versions":[]}`, ErrUnavailable},
	} {
		c := server(t, tc.status, tc.body, nil)
		if err := c.Verify(context.Background(), ref, caller); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

func TestVerifyRejectsNonIDsWithoutCallingTheVault(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	c, _ := New(srv.URL, nil)
	for _, bad := range []string{"", "sk_live_abc", "../etc/passwd", ref + "/../x", "password=hunter2"} {
		if err := c.Verify(context.Background(), bad, caller); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if called {
		t.Fatal("a reference that is not an id must never reach the network")
	}
}

func TestVerifyFailsClosedOnTimeoutAndUnreachable(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	c, _ := New(slow.URL, &http.Client{Timeout: 50 * time.Millisecond})
	start := time.Now()
	err := c.Verify(context.Background(), ref, caller)
	elapsed := time.Since(start)
	slow.Close()
	if !errors.Is(err, ErrUnavailable) || elapsed > 250*time.Millisecond {
		t.Fatalf("a slow vault must be unavailable, quickly: %v after %v", err, elapsed)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	c, _ = New(url, nil)
	if err := c.Verify(context.Background(), ref, caller); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
}

func TestErrorsDoNotLeakTheReferenceOrBody(t *testing.T) {
	c := server(t, 500, "internal secret detail 10.0.0.9", nil)
	err := c.Verify(context.Background(), ref, caller)
	if err == nil || strings.Contains(err.Error(), "10.0.0.9") {
		t.Fatalf("%v", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New("/relative", nil); err == nil {
		t.Error("relative URL accepted")
	}
	if _, err := New("http://vault:8080", &http.Client{}); err == nil {
		t.Error("a client without a timeout was accepted")
	}
}
