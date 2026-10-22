package middleware

import (
	"encoding/json"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
)

// errorBody is the service's error envelope: {"error", "error_code"}. The
// handler package writes the same two fields for every business and
// pre-flight error (handler.writeCoded); this file gives the transport-level
// responses — unknown route, unsupported method, recovered panic, readiness
// failure — the same shape, so a caller never has to special-case them.
type errorBody struct {
	Error     string `json:"error"`
	ErrorCode string `json:"error_code"`
}

// WriteError writes a JSON error envelope.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: msg, ErrorCode: code})
}

// allowedMethods lists the methods registered for a route whose path matches
// path, in a stable order. chi cannot be asked directly: a sub-router is mounted
// for every method, so Match answers true for all of them. Path parameters
// ({id}) match any single segment.
func allowedMethods(routes chi.Routes, path string) []string {
	want := splitPath(path)
	seen := map[string]bool{}
	_ = chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		have := splitPath(route)
		if len(have) != len(want) {
			return nil
		}
		for i := range have {
			param := strings.HasPrefix(have[i], "{") && strings.HasSuffix(have[i], "}")
			if !param && have[i] != want[i] {
				return nil
			}
		}
		seen[method] = true
		return nil
	})
	var out []string
	for _, m := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
	} {
		if seen[m] {
			out = append(out, m)
		}
	}
	return out
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// JSONErrors replaces chi's plain-text 404 and 405 responses with the JSON
// error envelope. Call it on the root router AFTER every route and sub-router
// has been registered: chi hands a custom handler down to the sub-routers that
// exist when it is set.
//
// Neither response consults authorization-svc or reveals anything about the
// registry: an unknown path is answered the same way for every caller. The 405
// keeps the Allow header chi's default handler sets (RFC 9110 §15.5.6).
func JSONErrors(r chi.Router) {
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, http.StatusNotFound, domain.ErrCodeNotFound, "route not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		if allow := allowedMethods(r, req.URL.Path); len(allow) > 0 {
			w.Header().Set("Allow", strings.Join(allow, ", "))
		}
		WriteError(w, http.StatusMethodNotAllowed, domain.ErrCodeMethodNotAllowed, "method not allowed for this route")
	})
}

// Recoverer turns a panic in any handler into a logged, JSON 500
// (INTERNAL_ERROR) instead of chi's bare 500 with an empty body. The panic
// value and stack go to the log only — never to the caller.
// http.ErrAbortHandler is re-raised, as net/http intends.
func Recoverer(log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rvr := recover(); rvr != nil {
					if rvr == http.ErrAbortHandler {
						panic(rvr)
					}
					log.Error("panic recovered",
						zap.Any("panic", rvr),
						zap.String("method", r.Method),
						zap.String("path", r.URL.Path),
						zap.ByteString("stack", debug.Stack()))
					WriteError(w, http.StatusInternalServerError, domain.ErrCodeInternal, "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
