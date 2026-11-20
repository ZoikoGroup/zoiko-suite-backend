package jurisdiction

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// TestHTTPValidator_KnownCodeSucceeds proves the real HTTP client against a
// fake jurisdiction-rules-svc that implements its actual contract: GET
// /v1/jurisdiction-resolutions?code=... returning 200 with a
// jurisdiction_id for a known code.
func TestHTTPValidator_KnownCodeSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jurisdiction-resolutions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("code") != "GB" {
			t.Fatalf("expected code=GB, got %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"jurisdiction_id":   "j-gb-1",
			"jurisdiction_code": "GB",
			"matched_via":       "INTERNAL",
		})
	}))
	defer srv.Close()

	logger, _ := zap.NewDevelopment()
	v := NewHTTPValidator(srv.URL, logger)
	if err := v.ValidateExists(context.Background(), "GB"); err != nil {
		t.Fatalf("expected a known code to validate, got %v", err)
	}
}

// TestHTTPValidator_UnknownCodeRejected proves the 404 path — the real
// jurisdiction-rules-svc contract for "no jurisdiction is known by that
// code" — maps to ErrMarketCodeNotFound, not a silent pass.
func TestHTTPValidator_UnknownCodeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	logger, _ := zap.NewDevelopment()
	v := NewHTTPValidator(srv.URL, logger)
	err := v.ValidateExists(context.Background(), "ZZ")
	if err == nil {
		t.Fatal("expected an unknown code to be rejected")
	}
	if !isMarketCodeNotFound(err) {
		t.Fatalf("expected ErrMarketCodeNotFound, got %v", err)
	}
}

// TestHTTPValidator_RetiredJurisdictionRejected proves the 410 Gone path
// (jurisdiction-rules-svc's RetiredError contract) is treated as not a
// valid market, not as a pass.
func TestHTTPValidator_RetiredJurisdictionRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	logger, _ := zap.NewDevelopment()
	v := NewHTTPValidator(srv.URL, logger)
	err := v.ValidateExists(context.Background(), "RETIRED-CODE")
	if !isMarketCodeNotFound(err) {
		t.Fatalf("expected ErrMarketCodeNotFound for a retired jurisdiction, got %v", err)
	}
}

// TestHTTPValidator_UnreachableFailsClosed proves the fail-closed guarantee:
// a connection failure must never be treated as a pass.
func TestHTTPValidator_UnreachableFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed before use, guaranteeing a connection failure

	logger, _ := zap.NewDevelopment()
	v := NewHTTPValidator(srv.URL, logger)
	err := v.ValidateExists(context.Background(), "GB")
	if err != ErrServiceUnavailable {
		t.Fatalf("expected ErrServiceUnavailable when unreachable, got %v", err)
	}
}

// TestHTTPValidator_UnexpectedStatusFailsClosed proves an unexpected
// response (neither 200, 404 nor 410) is never treated as a pass either.
func TestHTTPValidator_UnexpectedStatusFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	logger, _ := zap.NewDevelopment()
	v := NewHTTPValidator(srv.URL, logger)
	err := v.ValidateExists(context.Background(), "GB")
	if err != ErrServiceUnavailable {
		t.Fatalf("expected ErrServiceUnavailable for a 500 response, got %v", err)
	}
}

func isMarketCodeNotFound(err error) bool {
	for err != nil {
		if err == ErrMarketCodeNotFound {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
