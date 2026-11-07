package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The events the store writes carry the actor and the correlation id, so the
// handler must hand both to the store on the request context.
func TestAuditContextReachesTheStore(t *testing.T) {
	s := newStubStore()
	r := routerFor(s, &recordingPublisher{}, &stubAuthZ{})

	body, _ := jsonBody(unitBody("FIN", "c-1", nil))
	req, _ := http.NewRequest(http.MethodPost, "/v1/org/units", strings.NewReader(body))
	req.Header.Set("X-Principal-Id", "alice")
	req.Header.Set("X-Correlation-ID", "corr-123")
	if rr := serve(r, req); rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d %s", rr.Code, rr.Body.String())
	}
	if s.seenActor != "alice" || s.seenCorrelation != "corr-123" {
		t.Fatalf("the store must see the caller and correlation id, got %q / %q", s.seenActor, s.seenCorrelation)
	}
}

// Without a correlation header the handler still supplies one, so no event is
// ever written without an id to trace it by.
func TestAuditContextGeneratesACorrelationID(t *testing.T) {
	s := newStubStore()
	r := routerFor(s, &recordingPublisher{}, &stubAuthZ{})

	body, _ := jsonBody(unitBody("FIN", "c-2", nil))
	req, _ := http.NewRequest(http.MethodPost, "/v1/org/units", strings.NewReader(body))
	req.Header.Set("X-Principal-Id", "bob")
	if rr := serve(r, req); rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d", rr.Code)
	}
	if len(s.seenCorrelation) < 16 {
		t.Fatalf("a correlation id must be generated, got %q", s.seenCorrelation)
	}
}

func jsonBody(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}
