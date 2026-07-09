// Package handler exposes the REST API for schema-registry-svc.
package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/schema-registry-svc/internal/compat"
	"zoiko.io/schema-registry-svc/internal/domain"
	"zoiko.io/schema-registry-svc/internal/store"
)

type Handler struct {
	store store.Store
	log   *zap.Logger
}

func New(s store.Store, log *zap.Logger) *Handler {
	return &Handler{store: s, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/schemas", func(r chi.Router) {
		r.Get("/", h.ListEventNames)
		r.Post("/{eventName}/versions", h.RegisterVersion)
		r.Get("/{eventName}/versions", h.ListVersions)
		r.Get("/{eventName}/versions/latest", h.GetLatest)
		r.Get("/{eventName}/versions/{version}", h.GetVersion)
	})
}

// ── POST /v1/schemas/{eventName}/versions ───────────────────────────────────
//
// Registers the next version of eventName's payload schema. If a previous
// version exists, the proposed schema must be a backward-compatible
// evolution of it (see internal/compat) — a violation is a 409, not a 500,
// since it's a legitimate contract rejection, not a system failure.
func (h *Handler) RegisterVersion(w http.ResponseWriter, r *http.Request) {
	eventName := chi.URLParam(r, "eventName")
	if eventName == "" {
		writeError(w, http.StatusBadRequest, domain.ErrEventNameRequired.Error())
		return
	}

	var req domain.RegisterSchemaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.JSONSchema) == 0 {
		writeError(w, http.StatusBadRequest, domain.ErrSchemaRequired.Error())
		return
	}
	if !json.Valid(req.JSONSchema) {
		writeError(w, http.StatusBadRequest, domain.ErrSchemaMalformed.Error())
		return
	}

	ctx := r.Context()
	current, err := h.store.LatestVersion(ctx, eventName)
	if err != nil {
		h.log.Error("lookup latest version failed", zap.Error(err), zap.String("event_name", eventName))
		writeError(w, http.StatusServiceUnavailable, domain.ErrStoreUnavailable.Error())
		return
	}

	nextVersion := 1
	if current != nil {
		violations, err := compat.Check(current.JSONSchema, req.JSONSchema)
		if err != nil {
			writeError(w, http.StatusBadRequest, "schema shape error: "+err.Error())
			return
		}
		if len(violations) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      domain.ErrIncompatibleSchema.Error(),
				"violations": violations,
			})
			return
		}
		nextVersion = current.Version + 1
	}

	newSchema := &domain.EventSchema{
		EventName:    eventName,
		Version:      nextVersion,
		JSONSchema:   req.JSONSchema,
		RegisteredBy: r.Header.Get("X-Actor-Principal-ID"),
		RegisteredAt: time.Now().UTC(),
	}
	if err := h.store.Insert(ctx, newSchema); err != nil {
		h.log.Error("insert schema version failed", zap.Error(err), zap.String("event_name", eventName))
		writeError(w, http.StatusServiceUnavailable, domain.ErrStoreUnavailable.Error())
		return
	}

	h.log.Info("schema version registered",
		zap.String("event_name", eventName),
		zap.Int("version", nextVersion),
	)
	writeJSON(w, http.StatusCreated, newSchema)
}

// ── GET /v1/schemas/{eventName}/versions/latest ─────────────────────────────

func (h *Handler) GetLatest(w http.ResponseWriter, r *http.Request) {
	eventName := chi.URLParam(r, "eventName")
	schema, err := h.store.LatestVersion(r.Context(), eventName)
	h.respondOne(w, schema, err)
}

// ── GET /v1/schemas/{eventName}/versions/{version} ──────────────────────────

func (h *Handler) GetVersion(w http.ResponseWriter, r *http.Request) {
	eventName := chi.URLParam(r, "eventName")
	version, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "version must be an integer")
		return
	}
	schema, err := h.store.Version(r.Context(), eventName, version)
	h.respondOne(w, schema, err)
}

func (h *Handler) respondOne(w http.ResponseWriter, schema *domain.EventSchema, err error) {
	if err != nil {
		h.log.Error("lookup schema failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.ErrStoreUnavailable.Error())
		return
	}
	if schema == nil {
		writeError(w, http.StatusNotFound, domain.ErrVersionNotFound.Error())
		return
	}
	writeJSON(w, http.StatusOK, schema)
}

// ── GET /v1/schemas/{eventName}/versions ────────────────────────────────────

func (h *Handler) ListVersions(w http.ResponseWriter, r *http.Request) {
	eventName := chi.URLParam(r, "eventName")
	versions, err := h.store.Versions(r.Context(), eventName)
	if err != nil {
		h.log.Error("list versions failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.ErrStoreUnavailable.Error())
		return
	}
	if len(versions) == 0 {
		writeError(w, http.StatusNotFound, domain.ErrEventNotFound.Error())
		return
	}
	writeJSON(w, http.StatusOK, versions)
}

// ── GET /v1/schemas ──────────────────────────────────────────────────────────

func (h *Handler) ListEventNames(w http.ResponseWriter, r *http.Request) {
	names, err := h.store.EventNames(r.Context())
	if err != nil {
		h.log.Error("list event names failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.ErrStoreUnavailable.Error())
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, names)
}

// ── helpers ──────────────────────────────────────────────────────────────────

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
