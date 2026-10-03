package clients_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/financial-close-svc/internal/clients"
	"zoiko.io/financial-close-svc/internal/domain"
)

func gateClients(t *testing.T, u string) *clients.Clients {
	t.Helper()
	return newClients(t, "http://ledger.invalid").WithFinancialControlURL(u)
}

func serveGate(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/controls/v1/close-gate" ||
			r.URL.Query().Get("legal_entity_id") != "le-1" || r.URL.Query().Get("period_id") != "2026-01" ||
			r.Header.Get("X-Tenant-Id") != "tenant-1" || r.Header.Get("X-Principal-Id") != "principal-1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGetCloseGate_OpenBlockedUnconfigured(t *testing.T) {
	cases := []struct {
		name, body string
		open, conf bool
		blocking   int
	}{
		{"open", `{"open":true,"configured":true,"blocking_count":0,"items":[]}`, true, true, 0},
		{"blocked", `{"open":false,"configured":true,"blocking_count":2,"items":[{"control_code":"C1","status":"OPEN","reason":"x"}]}`, false, true, 2},
		{"unconfigured", `{"open":false,"configured":false,"blocking_count":0,"items":[]}`, false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveGate(t, http.StatusOK, tc.body)
			g, err := gateClients(t, srv.URL).GetCloseGate(t.Context(), "tenant-1", "principal-1", "le-1", "2026-01")
			if err != nil {
				t.Fatal(err)
			}
			if g.Open != tc.open || g.Configured != tc.conf || g.BlockingCount != tc.blocking {
				t.Fatalf("unexpected %+v", g)
			}
		})
	}
}

func TestGetCloseGate_Failures_ReturnSentinel(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()

	cases := map[string]string{
		"500":      serveGate(t, http.StatusInternalServerError, `{}`).URL,
		"bad json": serveGate(t, http.StatusOK, `not json`).URL,
		"refused":  "http://127.0.0.1:1",
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := gateClients(t, u).GetCloseGate(t.Context(), "tenant-1", "principal-1", "le-1", "2026-01")
			if !errors.Is(err, domain.ErrFinancialControlUnavailable) {
				t.Fatalf("want sentinel, got %v", err)
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		_, err := gateClients(t, slow.URL).GetCloseGate(ctx, "tenant-1", "principal-1", "le-1", "2026-01")
		if !errors.Is(err, domain.ErrFinancialControlUnavailable) {
			t.Fatalf("want sentinel, got %v", err)
		}
	})
}
