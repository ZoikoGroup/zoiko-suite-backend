package boardresolutions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeBoardResolutions mimics GET /v1/resolutions/{id}: 401 without tenant or
// principal, 404 for unknown ids, otherwise the stored record.
func fakeBoardResolutions(t *testing.T, records map[string]resolution, seen *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		if r.Header.Get("X-Tenant-Id") == "" || r.Header.Get("X-Principal-Id") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		id := r.URL.Path[len("/v1/resolutions/"):]
		rec, ok := records[id]
		if !ok || rec.TenantID != r.Header.Get("X-Tenant-Id") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(rec)
	}))
}

var caller = Caller{TenantID: "tenant-a", PrincipalID: "principal-1", LegalEntityID: "le-1", CorrelationID: "corr-1"}

func TestVerifyPassedResolution_Outcomes(t *testing.T) {
	records := map[string]resolution{
		"res-passed":   {ResolutionID: "res-passed", TenantID: "tenant-a", LegalEntityID: "le-1", Status: "PASSED"},
		"res-open":     {ResolutionID: "res-open", TenantID: "tenant-a", LegalEntityID: "le-1", Status: "OPEN"},
		"res-other-le": {ResolutionID: "res-other-le", TenantID: "tenant-a", LegalEntityID: "le-2", Status: "PASSED"},
		"res-other-t":  {ResolutionID: "res-other-t", TenantID: "tenant-b", LegalEntityID: "le-1", Status: "PASSED"},
	}
	var seen http.Header
	srv := fakeBoardResolutions(t, records, &seen)
	defer srv.Close()
	client := NewClient(srv.URL)

	cases := []struct {
		id   string
		want error
	}{
		{"res-passed", nil},
		{"res-missing", ErrResolutionNotFound},
		{"res-other-t", ErrResolutionNotFound}, // another tenant's resolution is invisible
		{"res-other-le", ErrResolutionEntityMismatch},
		{"res-open", ErrResolutionNotPassed},
	}
	for _, tc := range cases {
		err := client.VerifyPassedResolution(context.Background(), caller, tc.id, "le-1")
		if tc.want == nil && err != nil {
			t.Errorf("%s: expected success, got %v", tc.id, err)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: expected %v, got %v", tc.id, tc.want, err)
		}
	}
	if seen.Get("X-Principal-Id") != "principal-1" || seen.Get("X-Correlation-ID") != "corr-1" {
		t.Errorf("caller identity not forwarded: %v", seen)
	}
}

func TestVerifyPassedResolution_UnreachableFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL).VerifyPassedResolution(context.Background(), caller, "res-1", "le-1"); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("a 500 must fail closed as ErrServiceUnavailable, got %v", err)
	}
	if err := NewClient("http://127.0.0.1:1").VerifyPassedResolution(context.Background(), caller, "res-1", "le-1"); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("a connection failure must fail closed, got %v", err)
	}
}
