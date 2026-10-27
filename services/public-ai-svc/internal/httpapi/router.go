// Package httpapi exposes the public /public-ai/v1 API (Doc 05 §07) and the
// operational health probes (Doc 08 §10).
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/middleware"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/problem"
	"zoiko.io/public-ai-svc/internal/session"
	"zoiko.io/public-ai-svc/internal/store"
)

// ReadinessFunc reports whether the service may receive traffic. It must be
// false when signed safety config is missing or revocation cannot be
// enforced (Doc 08 §10).
type ReadinessFunc func() bool

// ConfigSource supplies the public widget configuration (Doc 05 §15).
type ConfigSource interface {
	PublicConfig() contract.ConfigResponse
}

// RouteResolver resolves an official route key to its approved URL.
type RouteResolver interface {
	ResolveRoute(key, market string) (string, bool)
}

// Deps are the server's collaborators. Nil members disable their routes.
type Deps struct {
	Store    store.Store
	Sessions *session.Service
	Answerer orchestrator.Answerer
	Config   ConfigSource
	Routes   RouteResolver
	Ready    ReadinessFunc
	// Limiter, when set, is applied to the public API (STEP 11).
	Limiter func(http.Handler) http.Handler
}

// Server holds the HTTP dependencies.
type Server struct {
	cfg  config.Config
	log  *zap.Logger
	deps Deps
}

// New builds a Server.
func New(cfg config.Config, log *zap.Logger, deps Deps) *Server {
	if log == nil {
		log = zap.NewNop()
	}
	if deps.Ready == nil {
		deps.Ready = func() bool { return false }
	}
	return &Server{cfg: cfg, log: log, deps: deps}
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
	r.Get("/health/ready", func(w http.ResponseWriter, _ *http.Request) { writeStatus(w, s.deps.Ready()) })

	r.Route("/public-ai/v1", func(api chi.Router) {
		api.Use(middleware.Origin(s.cfg.AllowedOrigins))
		if s.deps.Limiter != nil {
			api.Use(s.deps.Limiter)
		}
		api.Use(middleware.BodyLimit(s.cfg.MaxBodyBytes))
		api.Use(middleware.RequireJSON)
		api.Use(middleware.NoStore)
		// Coarse, generic public health: no provider or region names.
		api.Get("/health/public", func(w http.ResponseWriter, _ *http.Request) { writeStatus(w, s.deps.Ready()) })
		// SSE is behind stream_enabled=false in R0 (ADR-005).
		api.Get("/turns/{turn_id}/events", func(w http.ResponseWriter, r *http.Request) {
			problem.Write(w, r, problem.NotFound, "")
		})
		if s.deps.Config != nil {
			api.Get("/config", s.getConfig)
		}
		if s.deps.Sessions != nil {
			api.Post("/sessions", s.createSession)
			api.Delete("/sessions/{session_id}", s.deleteSession)
		}
		if s.deps.Sessions != nil && s.deps.Store != nil && s.deps.Answerer != nil {
			api.Post("/turns", s.postTurn)
			api.Post("/turns/{turn_id}/feedback", s.postFeedback)
		}
		if s.deps.Sessions != nil && s.deps.Store != nil && s.deps.Routes != nil {
			api.Post("/handoffs", s.postHandoff)
		}
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) { problem.Write(w, r, problem.NotFound, "") })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { problem.Write(w, r, problem.NotFound, "") })
	return r
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
