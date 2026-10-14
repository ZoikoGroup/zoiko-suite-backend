package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Every non-200 used to be ErrAuthzServiceUnavailable, so authorization-svc
// refusing a wrong-tenant question (403 tenant_scope_mismatch) surfaced to the
// user as 503 "authorization service unavailable".
func TestCheckAllowed_StatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusForbidden, ErrAuthorizationDenied},
		{http.StatusBadRequest, ErrAuthorizationRejected},
		{http.StatusUnauthorized, ErrAuthzServiceUnavailable}, // our workload identity refused
		{http.StatusServiceUnavailable, ErrAuthzServiceUnavailable},
		{http.StatusInternalServerError, ErrAuthzServiceUnavailable},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
		}))
		err := NewClient(srv.URL).CheckAllowed(context.Background(), "p", "le", "A")
		srv.Close()
		if !errors.Is(err, c.want) {
			t.Errorf("status %d: got %v, want %v", c.status, err, c.want)
		}
	}
}

// A 403 refusal is about the request's scope (a projection or registry answer
// can change it) and is not cached; a real DENIED decision is.
func TestCheckAllowed_RefusalNotCached_DecisionCached(t *testing.T) {
	var calls atomic.Int32
	status := http.StatusForbidden
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"decision_outcome":"DENIED"}`))
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	_ = c.CheckAllowed(context.Background(), "p", "le", "A")
	_ = c.CheckAllowed(context.Background(), "p", "le", "A")
	if calls.Load() != 2 {
		t.Fatalf("a 403 refusal was cached: %d calls", calls.Load())
	}
	status = http.StatusOK
	_ = c.CheckAllowed(context.Background(), "p", "le", "B")
	_ = c.CheckAllowed(context.Background(), "p", "le", "B")
	if calls.Load() != 3 {
		t.Fatalf("a DENIED decision was not cached: %d calls", calls.Load())
	}
}
