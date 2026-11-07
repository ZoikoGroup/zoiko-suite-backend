package apicontract

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestSetDeprecation(t *testing.T) {
	rec := httptest.NewRecorder()
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	sunset := time.Date(2027, 8, 1, 0, 0, 0, 0, time.UTC)
	SetDeprecation(rec, Deprecation{At: at, Sunset: sunset, Link: "https://docs.example/deprecations/v1-invoices"})

	if got := rec.Header().Get("Deprecation"); got != "@1785542400" {
		t.Fatalf("Deprecation = %q", got)
	}
	if got := rec.Header().Get("Sunset"); got != "Sun, 01 Aug 2027 00:00:00 GMT" {
		t.Fatalf("Sunset = %q", got)
	}
	if got := rec.Header().Get("Link"); got != `<https://docs.example/deprecations/v1-invoices>; rel="deprecation"` {
		t.Fatalf("Link = %q", got)
	}
}

func TestSetDeprecationOmitsUnsetFields(t *testing.T) {
	rec := httptest.NewRecorder()
	SetDeprecation(rec, Deprecation{})
	for _, h := range []string{"Deprecation", "Sunset", "Link"} {
		if got := rec.Header().Get(h); got != "" {
			t.Fatalf("%s = %q", h, got)
		}
	}
}
