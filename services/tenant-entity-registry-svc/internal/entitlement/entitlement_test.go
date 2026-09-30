package entitlement_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/entitlement"
)

// A real HTTP server standing in for commercial-account-svc's
// GET /v1/subscriptions/{id}.
func server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Principal-Id") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/") {
		case "active":
			_, _ = w.Write([]byte(`{"subscription_id":"active","status":"ACTIVE"}`))
		case "trial":
			_, _ = w.Write([]byte(`{"subscription_id":"trial","status":"EVALUATION"}`))
		case "pastdue":
			_, _ = w.Write([]byte(`{"subscription_id":"pastdue","status":"PAST_DUE"}`))
		case "broken":
			_, _ = w.Write([]byte(`not json`))
		case "error":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestHTTPChecker_EntitledStatusesProvision(t *testing.T) {
	srv := server(t)
	defer srv.Close()
	c := entitlement.NewHTTPChecker(srv.URL, zap.NewNop())
	for _, id := range []string{"active", "trial"} {
		if err := c.CheckProvisioning(context.Background(), id, "p-1"); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
}

func TestHTTPChecker_EverythingElseIsRefusedOrFailsClosed(t *testing.T) {
	srv := server(t)
	defer srv.Close()
	c := entitlement.NewHTTPChecker(srv.URL, zap.NewNop())
	for id, want := range map[string]error{
		"pastdue": entitlement.ErrNotEntitled,
		"missing": entitlement.ErrSubscriptionNotFound,
		"broken":  entitlement.ErrUnavailable,
		"error":   entitlement.ErrUnavailable,
	} {
		if err := c.CheckProvisioning(context.Background(), id, "p-1"); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", id, err, want)
		}
	}
	// An unreachable service is unavailability, never a pass.
	down := entitlement.NewHTTPChecker("http://127.0.0.1:1", zap.NewNop())
	if err := down.CheckProvisioning(context.Background(), "active", "p-1"); !errors.Is(err, entitlement.ErrUnavailable) {
		t.Errorf("unreachable: got %v", err)
	}
}
