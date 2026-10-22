package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/privacy-rights-svc/internal/middleware"
)

// The authorization client must present the same envelope the sibling privacy
// services send, so authorization-svc is never asked to admit an anonymous
// call and keeps working if it moves to strict envelope enforcement.

type seen struct {
	header http.Header
	body   map[string]string
}

type fakeAuthz struct {
	mu       sync.Mutex
	calls    []seen
	status   int
	response string
}

func (f *fakeAuthz) server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/authorize" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.calls = append(f.calls, seen{header: r.Header.Clone(), body: b})
		f.mu.Unlock()
		status := f.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(f.response))
	}))
	t.Cleanup(s.Close)
	return s
}

func granted() *fakeAuthz { return &fakeAuthz{response: `{"decision_outcome":"GRANTED"}`} }

func TestCheckAllowed_ForwardsTheIdentityAndEnvelopeHeaders(t *testing.T) {
	f := granted()
	c := NewClient(f.server(t).URL)
	ctx := middleware.WithCorrelation(middleware.WithTenant(context.Background(), "tenant-77"), "corr-abc")

	if err := c.CheckAllowed(ctx, "principal-9", "tenant-77", "PRIVACY_RIGHTS_REQUEST_CLOSE"); err != nil {
		t.Fatalf("CheckAllowed: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(f.calls))
	}
	h, b := f.calls[0].header, f.calls[0].body

	for header, want := range map[string]string{
		"X-Principal-Id":    "principal-9",
		"X-Legal-Entity-Id": "tenant-77",
		"X-Tenant-Id":       "tenant-77",
		"X-Correlation-Id":  "corr-abc",
		"X-Source-Channel":  "web",
	} {
		if got := h.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if _, err := uuid.Parse(h.Get("X-Request-Id")); err != nil {
		t.Errorf("X-Request-Id must be a UUID, got %q", h.Get("X-Request-Id"))
	}
	if _, err := uuid.Parse(h.Get("Idempotency-Key")); err != nil {
		t.Errorf("Idempotency-Key must be a UUID, got %q", h.Get("Idempotency-Key"))
	}
	// The body is unchanged.
	if b["principal_id"] != "principal-9" || b["legal_entity_id"] != "tenant-77" || b["action_type"] != "PRIVACY_RIGHTS_REQUEST_CLOSE" {
		t.Errorf("request body changed: %v", b)
	}
}

// Every check is a fresh question: request id and idempotency key are new each
// time, so a repeated check is never answered from an earlier decision.
func TestCheckAllowed_RequestIDAndIdempotencyKeyAreFreshPerCheck(t *testing.T) {
	f := granted()
	c := NewClient(f.server(t).URL)
	ctx := middleware.WithTenant(context.Background(), "tenant-1")
	for i := 0; i < 2; i++ {
		if err := c.CheckAllowed(ctx, "p", "tenant-1", "A"); err != nil {
			t.Fatal(err)
		}
	}
	a, b := f.calls[0].header, f.calls[1].header
	if a.Get("X-Request-Id") == b.Get("X-Request-Id") {
		t.Error("X-Request-Id must differ between checks")
	}
	if a.Get("Idempotency-Key") == b.Get("Idempotency-Key") {
		t.Error("Idempotency-Key must differ between checks")
	}
}

// With no correlation id on the context (reads) a fresh one is generated, and
// with no tenant on the context the verified tenant passed as the scope is used.
func TestCheckAllowed_FallbacksForCorrelationAndTenant(t *testing.T) {
	f := granted()
	c := NewClient(f.server(t).URL)
	if err := c.CheckAllowed(context.Background(), "p", "tenant-from-scope", "A"); err != nil {
		t.Fatal(err)
	}
	h := f.calls[0].header
	if _, err := uuid.Parse(h.Get("X-Correlation-Id")); err != nil {
		t.Errorf("fallback X-Correlation-ID must be a UUID, got %q", h.Get("X-Correlation-Id"))
	}
	if got := h.Get("X-Tenant-Id"); got != "tenant-from-scope" {
		t.Errorf("X-Tenant-Id fallback = %q, want the scope", got)
	}
}

// The context tenant is the VERIFIED one and wins over the scope argument.
func TestCheckAllowed_TenantHeaderComesFromTheContext(t *testing.T) {
	f := granted()
	c := NewClient(f.server(t).URL)
	ctx := middleware.WithTenant(context.Background(), "verified-tenant")
	if err := c.CheckAllowed(ctx, "p", "other-scope", "A"); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[0].header.Get("X-Tenant-Id"); got != "verified-tenant" {
		t.Errorf("X-Tenant-Id = %q, want the context tenant", got)
	}
}

// Decision handling is unchanged by the added headers.
func TestCheckAllowed_DecisionHandlingUnchanged(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		response string
		want     error
	}{
		{"granted", 200, `{"decision_outcome":"GRANTED"}`, nil},
		{"denied", 200, `{"decision_outcome":"DENIED"}`, ErrAuthorizationDenied},
		{"any non-GRANTED outcome is a denial", 200, `{"decision_outcome":"PENDING"}`, ErrAuthorizationDenied},
		{"non-200 fails closed as unavailable", 401, `{}`, ErrAuthzServiceUnavailable},
		{"server error fails closed as unavailable", 500, `{}`, ErrAuthzServiceUnavailable},
		{"unparseable body fails closed as unavailable", 200, `not json`, ErrAuthzServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAuthz{status: tc.status, response: tc.response}
			c := NewClient(f.server(t).URL)
			err := c.CheckAllowed(middleware.WithTenant(context.Background(), "t"), "p", "t", "A")
			if !errors.Is(err, tc.want) && !(err == nil && tc.want == nil) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCheckAllowed_UnreachableServiceFailsClosed(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	url := s.URL
	s.Close() // nothing listens any more
	err := NewClient(url).CheckAllowed(middleware.WithTenant(context.Background(), "t"), "p", "t", "A")
	if !errors.Is(err, ErrAuthzServiceUnavailable) {
		t.Fatalf("got %v, want ErrAuthzServiceUnavailable", err)
	}
}
