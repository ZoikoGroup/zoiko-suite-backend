package clients_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/financial-close-svc/internal/domain"
)

// The request must be one general-ledger-svc's unposted-events population
// accepts: it rejects any parameter outside legal_entity_id, created_before,
// limit and cursor with a 400, and authorizes the forwarded principal.
func TestGetPostingBacklog_RequestShape(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte(`{"records":[],"next_cursor":"","watermark":"gl6:0","declared_totals":{"row_count":0,"totals":{}}}`))
	}))
	defer srv.Close()

	cutoff := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	backlog, err := newClients(t, srv.URL).GetPostingBacklog(t.Context(), "tenant-1", "controller-1", "le-1", cutoff)
	if err != nil {
		t.Fatalf("GetPostingBacklog: %v", err)
	}
	if backlog.Count != 0 || backlog.TooLarge {
		t.Fatalf("empty population read as %+v", backlog)
	}
	if got.URL.Path != "/v1/control-populations/unposted-events" {
		t.Fatalf("path %q", got.URL.Path)
	}
	q := got.URL.Query()
	if len(q) != 3 || q.Get("legal_entity_id") != "le-1" || q.Get("created_before") != "2026-11-01T00:00:00Z" || q.Get("limit") == "" {
		t.Fatalf("query %v: must be exactly legal_entity_id, created_before (RFC3339 UTC) and limit", q)
	}
	if got.Header.Get("X-Tenant-Id") != "tenant-1" || got.Header.Get("X-Principal-Id") != "controller-1" {
		t.Fatalf("tenant/principal not forwarded: %v", got.Header)
	}
}

// The count is the population's declared total, not the page length: the
// page is capped at a few samples, the backlog is not.
func TestGetPostingBacklog_CountComesFromDeclaredTotals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"records":[
			{"record_id":"e1","reference":"inv-102-issued","amount":"0","currency":"XXX","date":"2026-10-28",
			 "attributes":{"status":"FAILED","kind":"EVENT","failure_reason":"journal write failed","age_days":"4"}}],
			"next_cursor":"ZTE","watermark":"gl6:1","declared_totals":{"row_count":37,"totals":{}}}`))
	}))
	defer srv.Close()

	backlog, err := newClients(t, srv.URL).GetPostingBacklog(t.Context(), "tenant-1", "p", "le-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Count != 37 {
		t.Fatalf("count %d, want the declared 37", backlog.Count)
	}
	if len(backlog.Samples) != 1 || backlog.Samples[0] != (domain.PostingBacklogItem{
		Reference: "inv-102-issued", Status: "FAILED", FailureReason: "journal write failed"}) {
		t.Fatalf("samples %+v", backlog.Samples)
	}
}

func TestGetPostingBacklog_StatusMapping(t *testing.T) {
	cases := []struct {
		status  int
		wantErr error
		tooBig  bool
	}{
		{http.StatusForbidden, domain.ErrPostingBacklogForbidden, false},
		{http.StatusUnprocessableEntity, nil, true},
		{http.StatusServiceUnavailable, domain.ErrGLServiceUnavailable, false},
		{http.StatusBadRequest, domain.ErrGLServiceUnavailable, false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
		}))
		backlog, err := newClients(t, srv.URL).GetPostingBacklog(t.Context(), "tenant-1", "p", "le-1", time.Now())
		srv.Close()
		if !errors.Is(err, c.wantErr) && !(c.wantErr == nil && err == nil) {
			t.Errorf("status %d: err %v, want %v", c.status, err, c.wantErr)
		}
		if backlog.TooLarge != c.tooBig {
			t.Errorf("status %d: TooLarge %v, want %v", c.status, backlog.TooLarge, c.tooBig)
		}
	}
}

func TestGetPostingBacklog_UnreachableIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	if _, err := newClients(t, url).GetPostingBacklog(t.Context(), "tenant-1", "p", "le-1", time.Now()); !errors.Is(err, domain.ErrGLServiceUnavailable) {
		t.Fatalf("unreachable GL must be an error, never an empty backlog: %v", err)
	}
}
