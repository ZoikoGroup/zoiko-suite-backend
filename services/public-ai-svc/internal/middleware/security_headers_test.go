package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Doc 06 §11: every response carries the conservative header set,
// including Cross-Origin-Resource-Policy: same-origin.
func TestSecurityHeaders(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/public-ai/v1/config", nil))
	for k, want := range map[string]string{
		"Cross-Origin-Resource-Policy": "same-origin",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
