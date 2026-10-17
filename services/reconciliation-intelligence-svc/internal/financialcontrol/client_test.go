package financialcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"math/big"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestGetActiveAbsoluteTolerance_HappyPath_ReturnsLatestVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant-Id") != "tenant-1" || r.Header.Get("X-Principal-Id") != "principal-1" {
			t.Errorf("expected tenant/principal headers to be forwarded, got %q/%q", r.Header.Get("X-Tenant-Id"), r.Header.Get("X-Principal-Id"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"tolerance_version": 1, "absolute_tolerance": "25"},
				{"tolerance_version": 2, "absolute_tolerance": "75"}, // widened later — the current one
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, zap.NewNop())
	tol, err := c.GetActiveAbsoluteTolerance(context.Background(), "tenant-1", "principal-1", "le-1", "SOME_METRIC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tol.Cmp(big.NewRat(75, 1)) != 0 {
		t.Fatalf("expected the highest-version (current) policy's tolerance 75, got %v", tol)
	}
}

// ZS-DATA-001 D06: the governed tolerance arrives as a decimal string and
// must reach the comparison exactly — a float64 parse would turn this
// 17-significant-digit value into a nearby binary approximation.
func TestGetActiveAbsoluteTolerance_PreservesExactDecimal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{"tolerance_version": 1, "absolute_tolerance": "12345678901234.567"}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, zap.NewNop())
	tol, err := c.GetActiveAbsoluteTolerance(context.Background(), "tenant-1", "principal-1", "le-1", "SOME_METRIC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, _ := new(big.Rat).SetString("12345678901234.567")
	if tol.Cmp(want) != 0 {
		t.Fatalf("tolerance lost precision: got %s, want 12345678901234.567", tol.FloatString(6))
	}
}

func TestGetActiveAbsoluteTolerance_NegativeTolerance_FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{"tolerance_version": 1, "absolute_tolerance": "-5"}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, zap.NewNop())
	_, err := c.GetActiveAbsoluteTolerance(context.Background(), "tenant-1", "principal-1", "le-1", "SOME_METRIC")
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable on a negative tolerance, got %v", err)
	}
}

func TestGetActiveAbsoluteTolerance_NoPolicyConfigured_RefusesRatherThanGuesses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, zap.NewNop())
	_, err := c.GetActiveAbsoluteTolerance(context.Background(), "tenant-1", "principal-1", "le-1", "SOME_METRIC")
	if !errors.Is(err, ErrToleranceNotConfigured) {
		t.Fatalf("expected ErrToleranceNotConfigured, got %v", err)
	}
}

func TestGetActiveAbsoluteTolerance_ServiceUnreachable_FailsClosed(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", zap.NewNop()) // nothing listens here
	_, err := c.GetActiveAbsoluteTolerance(context.Background(), "tenant-1", "principal-1", "le-1", "SOME_METRIC")
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable, got %v", err)
	}
}

func TestGetActiveAbsoluteTolerance_NonOKStatus_FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, zap.NewNop())
	_, err := c.GetActiveAbsoluteTolerance(context.Background(), "tenant-1", "principal-1", "le-1", "SOME_METRIC")
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable on a 500, got %v", err)
	}
}

func TestGetActiveAbsoluteTolerance_MalformedTolerance_FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{"tolerance_version": 1, "absolute_tolerance": "not-a-number"}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, zap.NewNop())
	_, err := c.GetActiveAbsoluteTolerance(context.Background(), "tenant-1", "principal-1", "le-1", "SOME_METRIC")
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable on a malformed tolerance value, got %v", err)
	}
}

func TestGetActiveAbsoluteTolerance_ScopesByLegalEntityAndMetric(t *testing.T) {
	var gotLegalEntity, gotMetric string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLegalEntity = r.URL.Query().Get("legal_entity_id")
		gotMetric = r.URL.Query().Get("metric")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{"tolerance_version": 1, "absolute_tolerance": "1"}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, zap.NewNop())
	if _, err := c.GetActiveAbsoluteTolerance(context.Background(), "tenant-1", "principal-1", "le-42", "A_METRIC"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotLegalEntity != "le-42" || gotMetric != "A_METRIC" {
		t.Fatalf("expected the request scoped to le-42/A_METRIC, got %q/%q", gotLegalEntity, gotMetric)
	}
}
