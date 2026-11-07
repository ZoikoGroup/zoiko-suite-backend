package apicontract

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestIDMiddlewareEchoesValidClientValue(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r)
		if RequestIDFromContext(r.Context()) != seen {
			t.Fatalf("context id differs from request id")
		}
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderRequestID, "client-req_12345678")
	h.ServeHTTP(rec, req)

	if seen != "client-req_12345678" {
		t.Fatalf("seen = %q", seen)
	}
	if got := rec.Header().Get(HeaderRequestID); got != "client-req_12345678" {
		t.Fatalf("response id = %q", got)
	}
	if got := rec.Header().Get(HeaderLegacyRequestID); got != "client-req_12345678" {
		t.Fatalf("legacy response id = %q", got)
	}
}

func TestRequestIDMiddlewareRejectsMalformedAndGenerates(t *testing.T) {
	cases := []string{"", "short", "bad value with spaces", "semi;colon12345678", string(make([]byte, 200))}
	for _, raw := range cases {
		h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := RequestIDFrom(r)
			if !ValidRequestID(id) || len(id) < 8 || len(id) > 128 {
				t.Fatalf("generated id %q is not valid", id)
			}
			if id == raw {
				t.Fatalf("malformed id was echoed: %q", raw)
			}
		}))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		if raw != "" {
			req.Header.Set(HeaderRequestID, raw)
		}
		h.ServeHTTP(rec, req)
	}
}

func TestRequestIDFallsBackToLegacyHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderLegacyRequestID, "legacy-req-12345678")
	RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := RequestIDFrom(r); got != "legacy-req-12345678" {
			t.Fatalf("id = %q", got)
		}
	})).ServeHTTP(rec, req)
}
