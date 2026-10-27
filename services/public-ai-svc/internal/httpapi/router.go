// Package httpapi exposes the public /public-ai/v1 API (Doc 05 §07) and the
// operational health probes (Doc 08 §10).
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/middleware"
	"zoiko.io/public-ai-svc/internal/problem"
)

// ReadinessFunc reports whether the service may receive traffic. It must be
// false when signed safety config is missing or revocation cannot be
// enforced (Doc 08 §10).
type ReadinessFunc func() bool

// Server holds the HTTP dependencies.
type Server struct {
	cfg   config.Config
	log   *zap.Logger
	ready ReadinessFunc
}

// New builds a Server. ready may be nil (treated as not ready).
func New(cfg config.Config, log *zap.Logger, ready ReadinessFunc) *Server {
	if log == nil {
		log = zap.NewNop()
	}
	if ready == nil {
		ready = func() bool { return false }
	}
	return &Server{cfg: cfg, log: log, ready: ready}
}

// Router returns the root handler.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.AccessLog(s.log))
	r.Use(middleware.Recover(s.log))

	// Internal probes: never call the model or commercial data (Doc 08 §10).
	r.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) { writeStatus(w, true) })
	r.Get("/health/startup", func(w http.ResponseWriter, _ *http.Request) { writeStatus(w, true) })
	r.Get("/health/ready", func(w http.ResponseWriter, _ *http.Request) { writeStatus(w, s.ready()) })

	r.Route("/public-ai/v1", func(api chi.Router) {
		api.Use(middleware.Origin(s.cfg.AllowedOrigins))
		api.Use(middleware.BodyLimit(s.cfg.MaxBodyBytes))
		api.Use(middleware.RequireJSON)
		api.Use(middleware.NoStore)
		// Coarse, generic public health: no provider or region names.
		api.Get("/health/public", func(w http.ResponseWriter, _ *http.Request) { writeStatus(w, s.ready()) })
		s.mountPublicAPI(api)
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) { problem.Write(w, r, problem.NotFound, "") })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { problem.Write(w, r, problem.NotFound, "") })
	return r
}

// mountPublicAPI is extended by later steps (sessions, turns, config).
func (s *Server) mountPublicAPI(api chi.Router) {
	// SSE is behind stream_enabled=false in R0 (ADR-005).
	api.Get("/turns/{turn_id}/events", func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, r, problem.NotFound, "")
	})
}

func writeStatus(w http.ResponseWriter, ok bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	status, body := http.StatusOK, "ok"
	if !ok {
		status, body = http.StatusServiceUnavailable, "unavailable"
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": body})
}
