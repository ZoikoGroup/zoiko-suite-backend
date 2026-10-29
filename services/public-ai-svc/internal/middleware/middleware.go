// Package middleware holds the edge controls that run before any handler
// (Doc 04 §08 step 1, Doc 05 §06, Doc 06 §11–§12).
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"zoiko.io/public-ai-svc/internal/problem"
	"zoiko.io/public-ai-svc/internal/telemetry"
)

// RequestID always issues a fresh server-side request id (Doc 05 §06: the
// server issues X-Request-ID). Inbound values are ignored so a client cannot
// inject identifiers into our logs.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", NewRequestID())
		next.ServeHTTP(w, r)
	})
}

// NewRequestID returns an opaque "req_" identifier.
func NewRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

// Recover turns a panic into a generic 500 problem without leaking details.
func Recover(log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic recovered", zap.String("request_id", w.Header().Get("X-Request-ID")))
					problem.Write(w, r, problem.Internal, "")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets conservative response headers on every response.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

// BodyLimit rejects bodies above max with 413 before the handler runs, and
// caps streamed bodies that do not declare a Content-Length.
func BodyLimit(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > max {
				problem.Write(w, r, problem.PayloadTooLarge, "")
				return
			}
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, max)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Allowed request headers for the browser widget.
const allowedHeaders = "Content-Type, Idempotency-Key, X-Public-Chat-Session, traceparent"

// Origin enforces the first-party origin allowlist on the public API and
// answers CORS preflight. Requests without an Origin header are rejected:
// the only legitimate caller of /public-ai/v1 is the first-party widget.
func Origin(allowed []string) func(http.Handler) http.Handler {
	allow := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allow[strings.TrimRight(a, "/")] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if !allow[origin] {
				problem.Write(w, r, problem.OriginDenied, "")
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After, ETag")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE")
				h.Set("Access-Control-Allow-Headers", allowedHeaders)
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Host rejects requests for an unexpected Host header when an allowlist is
// configured (Doc 04 §08: validate Origin/Host before any work).
func Host(allowed []string) func(http.Handler) http.Handler {
	allow := map[string]bool{}
	for _, h := range allowed {
		allow[strings.ToLower(h)] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(allow) > 0 && !allow[strings.ToLower(r.Host)] {
				problem.Write(w, r, problem.OriginDenied, "")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireJSON enforces application/json on requests that carry a body.
func RequireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
				problem.Write(w, r, problem.UnsupportedMediaType, "")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// NoStore marks responses as non-cacheable (sessions and turns, Doc 05 §06).
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

var traceparentRe = regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-[0-9a-f]{2}$`)

// TraceID returns the trace id of a W3C traceparent header only when it is
// well formed and non-zero (Doc 05 §06: propagate only after validation).
func TraceID(header string) string {
	m := traceparentRe.FindStringSubmatch(strings.TrimSpace(header))
	if m == nil || strings.Trim(m[1], "0") == "" || strings.Trim(m[2], "0") == "" {
		return ""
	}
	return m[1]
}

// AccessLog logs one line per request with allowlisted fields only. It never
// logs bodies, query strings, headers or the session grant (Doc 05 §24).
func AccessLog(log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			route := "unmatched"
			if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
				route = rc.RoutePattern()
			}
			log.Info("http_request",
				zap.String("request_id", w.Header().Get("X-Request-ID")),
				zap.String("trace_id", TraceID(r.Header.Get("traceparent"))),
				zap.String("method", r.Method),
				zap.String("route", route),
				zap.Int("status", rec.status),
				zap.Int64("latency_ms", time.Since(start).Milliseconds()),
			)
		})
	}
}

// Trace opens the server span for a request, continuing a valid W3C
// traceparent. It records the method, route pattern, status and request id
// only: no query string, headers, body or session grant (Doc 08 §14).
func Trace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := telemetry.Tracer().Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))
		route := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = rc.RoutePattern()
		}
		span.SetName(r.Method + " " + route)
		span.SetAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", route),
			attribute.Int("http.response.status_code", rec.status),
			attribute.String("request_id", w.Header().Get("X-Request-ID")),
		)
		if rec.status >= 500 {
			telemetry.Fail(span, strconv.Itoa(rec.status))
		}
	})
}
