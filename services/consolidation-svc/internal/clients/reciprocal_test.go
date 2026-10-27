package clients_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/consolidation-svc/internal/clients"
	"zoiko.io/consolidation-svc/internal/domain"
)

// reciprocalServer answers GET /v1/intercompany/reciprocal-balances the way
// intercompany-accounting-svc does, and fails the request if the pair, period
// or caller identity is not forwarded exactly.
func reciprocalServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/intercompany/reciprocal-balances" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
			return
		}
		q := r.URL.Query()
		if q.Get("entity_a") != "le-a" || q.Get("entity_b") != "le-b" || q.Get("period") != "2026-07" {
			http.Error(w, "unexpected pair/period "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Tenant-Id") != "tenant-1" || r.Header.Get("X-Principal-Id") != "principal-1" {
			http.Error(w, "unexpected identity", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchReciprocalBalances_ForwardsPairPeriodAndIdentity(t *testing.T) {
	srv := reciprocalServer(t, http.StatusOK, `{
		"entity_a": "le-a",
		"entity_b": "le-b",
		"fiscal_period": "2026-07",
		"by_match_status": [{
			"match_status": "MATCHED",
			"entity_a_to_entity_b": [{"currency_code": "USD", "amount": 1234.5678}],
			"entity_b_to_entity_a": [],
			"net": [{"currency_code": "USD", "amount": 1234.5678}]
		}]
	}`)

	got, err := clients.New("", srv.URL, zap.NewNop()).
		FetchReciprocalBalances(context.Background(), "tenant-1", "principal-1", "le-a", "le-b", "2026-07")
	if err != nil {
		t.Fatalf("FetchReciprocalBalances: %v", err)
	}
	if got.EntityA != "le-a" || got.EntityB != "le-b" || got.FiscalPeriod != "2026-07" {
		t.Fatalf("pair/period lost: %+v", got)
	}
	if len(got.ByMatchStatus) != 1 || got.ByMatchStatus[0].MatchStatus != "MATCHED" {
		t.Fatalf("status split lost: %+v", got.ByMatchStatus)
	}
	if got.ByMatchStatus[0].EntityAToEntityB[0].Amount != domain.Decimal("1234.5678") {
		t.Errorf("amount lost precision: %q", got.ByMatchStatus[0].EntityAToEntityB[0].Amount)
	}
	if got.ByMatchStatus[0].EntityBToEntityA == nil {
		t.Error("empty direction must survive as [], not become nil")
	}
}

func TestFetchReciprocalBalances_Non200IsUnavailable(t *testing.T) {
	srv := reciprocalServer(t, http.StatusInternalServerError, `{"error_code":"internal"}`)
	_, err := clients.New("", srv.URL, zap.NewNop()).
		FetchReciprocalBalances(context.Background(), "tenant-1", "principal-1", "le-a", "le-b", "2026-07")
	if !errors.Is(err, domain.ErrIntercompanyUnavailable) {
		t.Fatalf("expected ErrIntercompanyUnavailable, got %v", err)
	}
}

func TestFetchReciprocalBalances_ForbiddenIsAuthzDenied(t *testing.T) {
	srv := reciprocalServer(t, http.StatusForbidden, `{"error_code":"forbidden"}`)
	_, err := clients.New("", srv.URL, zap.NewNop()).
		FetchReciprocalBalances(context.Background(), "tenant-1", "principal-1", "le-a", "le-b", "2026-07")
	if !errors.Is(err, domain.ErrAuthorizationDenied) {
		t.Fatalf("expected ErrAuthorizationDenied, got %v", err)
	}
}

func TestFetchReciprocalBalances_MalformedAmountFails(t *testing.T) {
	srv := reciprocalServer(t, http.StatusOK, `{
		"entity_a": "le-a", "entity_b": "le-b", "fiscal_period": "2026-07",
		"by_match_status": [{
			"match_status": "MATCHED",
			"entity_a_to_entity_b": [{"currency_code": "USD", "amount": "1/3"}],
			"entity_b_to_entity_a": [], "net": []
		}]
	}`)
	if _, err := clients.New("", srv.URL, zap.NewNop()).
		FetchReciprocalBalances(context.Background(), "tenant-1", "principal-1", "le-a", "le-b", "2026-07"); err == nil {
		t.Fatal("a malformed upstream amount must fail the read rather than become zero")
	}
}
