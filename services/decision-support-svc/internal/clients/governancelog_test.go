package clients

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	svcenvelope "zoiko.io/decision-support-svc/internal/envelope"
)

// D2-2.F07: the decision log answers a page ({"records": [...]}). Decoding a
// bare array failed on every call, so every recommendation degraded to
// NO_HISTORY. The read names this service's principal and carries the full
// envelope with a read purpose.
func TestListPriorDecisions_DecodesThePageAndSendsTheFullEnvelope(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"records":[{"outcome":"GRANTED"},{"outcome":"DENIED"},{"outcome":"GRANTED"}],"total_count":3,"next_cursor":"","limit":3,"offset":0}`))
	}))
	defer srv.Close()

	c := NewGovernanceLogClientWithHTTPClient(srv.URL, srv.Client(), "svc-decision-support")
	ctx := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{CorrelationID: "corr-rec"})
	ds, err := c.ListPriorDecisions(ctx, "tenant-1", "le-1", "CONTRACT_APPROVE", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 3 || ds[0].Outcome != "GRANTED" || ds[1].Outcome != "DENIED" {
		t.Fatalf("decoded %+v", ds)
	}
	q := got.URL.Query()
	if q.Get("entity") != "le-1" || q.Get("action") != "CONTRACT_APPROVE" || q.Get("limit") != "3" {
		t.Fatalf("query %s", got.URL.RawQuery)
	}
	for k, want := range map[string]string{
		"X-Tenant-Id": "tenant-1", "X-Principal-Id": "svc-decision-support", "X-Correlation-ID": "corr-rec",
		"X-Source-Channel": "system", "X-Purpose-Context": GovernanceLogPurpose, "X-Legal-Entity-Id": "le-1",
	} {
		if got.Header.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.Header.Get(k), want)
		}
	}
	if got.Header.Get("X-Request-Id") == "" {
		t.Error("X-Request-Id missing")
	}
}

func TestListPriorDecisions_RefusalsAndMalformedPagesAreErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"401 identity refused":   {http.StatusUnauthorized, `{}`},
		"403 read not granted":   {http.StatusForbidden, `{}`},
		"bare array (old shape)": {http.StatusOK, `[{"outcome":"GRANTED"}]`},
		"no records member":      {http.StatusOK, `{"total_count":0}`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		if _, err := NewGovernanceLogClient(srv.URL).ListPriorDecisions(context.Background(), "t", "le", "A", 5); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		srv.Close()
	}

	// An empty page is an empty history, not an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"records":[],"total_count":0}`))
	}))
	defer srv.Close()
	ds, err := NewGovernanceLogClient(srv.URL).ListPriorDecisions(context.Background(), "t", "le", "A", 5)
	if err != nil || len(ds) != 0 {
		t.Fatalf("empty page: %v %v", ds, err)
	}
}
