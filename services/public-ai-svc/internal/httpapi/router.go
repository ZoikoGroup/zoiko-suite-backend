// Package httpapi exposes the public /public-ai/v1 API (Doc 05 §07) and the
// operational health probes (Doc 08 §10).
package httpapi

import (
	"encoding/json"
	"net/http"
	"zoiko.io/public-ai-svc/internal/metrics"

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
	// Flags, when set, gates GET /menu and the structured turn fields.
	Flags FlagSource
	// Ready gates traffic: releases verified and fresh, flags readable and
	// not draining. It must NOT include the kill switch: a disabled
	// assistant stays ready and answers with the typed KILL_SWITCH outcome
	// (Doc 08 §26, OPR-011), so a kill is never mistaken for a release
	// failure by the load balancer or the release_ready alert.
	Ready ReadinessFunc
	// ReleaseReady, when set, feeds public_ai_release_ready from the release
	// runtime alone (Doc 08 §10). Without it the metric follows Ready.
	ReleaseReady ReadinessFunc
	// Started reports whether the first release and flag load completed;
	// /health/startup is 503 until then. Nil means started.
	Started ReadinessFunc
	// ChatEnabled reports whether chat is on (kill switch off). It is
	// published as capabilities.chat_enabled in GET /config. When nil the
	// effective public_ai_enabled flag from Flags is used, if any.
	ChatEnabled func() bool
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
	if deps.ReleaseReady == nil {
		deps.ReleaseReady = deps.Ready
	}
	if deps.Started == nil {
		deps.Started = func() bool { return true }
	}
	if deps.ChatEnabled == nil && deps.Flags != nil {
		fl := deps.Flags
		deps.ChatEnabled = func() bool { return fl.Values().PublicAIEnabled }
	}
	if deps.Config != nil && deps.ChatEnabled != nil {
		deps.Config = chatEnabledConfig{src: deps.Config, enabled: deps.ChatEnabled}
	}
	return &Server{cfg: cfg, log: log, deps: deps}
}

// Router returns the root handler.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Trace)
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.AccessLog(s.log))
	r.Use(middleware.Recover(s.log))

	// Internal probes and metrics: on the public listener only when no
	// separate admin listener is configured (Doc 06 §12).
	if s.cfg.AdminAddr == "" {
		s.mountInternal(r)
	}

	r.Route("/public-ai/v1", func(api chi.Router) {
		api.Use(middleware.Host(s.cfg.AllowedHosts))
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
			api.Get("/menu", s.getMenu)
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
		if s.deps.Sessions != nil && s.deps.Store != nil {
			// Doc 02 §27 browser-only analytics (chat_opened, source_opened).
			api.Post("/analytics", s.postAnalytics)
		}
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) { problem.Write(w, r, problem.NotFound, "") })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { problem.Write(w, r, problem.NotFound, "") })
	return r
}

// AdminRouter serves the internal endpoints (/health/*, /metrics) for the
// admin listener (PUBLIC_AI_ADMIN_ADDR, Doc 06 §12). Everything else is 404.
func (s *Server) AdminRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.Recover(s.log))
	s.mountInternal(r)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { problem.Write(w, r, problem.NotFound, "") })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { problem.Write(w, r, problem.NotFound, "") })
	return r
}

// mountInternal adds the operational probes and metrics. They never call
// the model or commercial data (Doc 08 §10).
func (s *Server) mountInternal(r chi.Router) {
	r.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) { writeStatus(w, true) })
	r.Get("/health/startup", func(w http.ResponseWriter, _ *http.Request) { writeStatus(w, s.deps.Started()) })
	r.Get("/health/ready", func(w http.ResponseWriter, _ *http.Request) { writeStatus(w, s.deps.Ready()) })
	// Doc 08 §14: Prometheus metrics, outside the public API (never under
	// /public-ai/v1) and with low-cardinality labels only.
	r.Get("/metrics", func(w http.ResponseWriter, req *http.Request) {
		metrics.SetReleaseReady(s.deps.ReleaseReady())
		metrics.Handler().ServeHTTP(w, req)
	})
}

// chatEnabledConfig adds capabilities.chat_enabled to the public config so
// the widget can show the kill-switch state without sending a turn.
type chatEnabledConfig struct {
	src     ConfigSource
	enabled func() bool
}

func (c chatEnabledConfig) PublicConfig() contract.ConfigResponse {
	cfg := c.src.PublicConfig()
	on := c.enabled()
	cfg.Capabilities.ChatEnabled = &on
	return cfg
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
