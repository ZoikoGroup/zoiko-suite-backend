package apicontract

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDefaultCacheSetsNoStore(t *testing.T) {
	rec := httptest.NewRecorder()
	DefaultCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("cache control = %q", got)
	}
}

func TestDefaultCachePreservesEndpointOverride(t *testing.T) {
	rec := httptest.NewRecorder()
	h := DefaultCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetPublicCache(w, "public, max-age=60")
	}))
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("cache control = %q", got)
	}
}
