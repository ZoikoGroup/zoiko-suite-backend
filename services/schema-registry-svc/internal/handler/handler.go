// Package handler exposes the REST API for schema-registry-svc.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/schema-registry-svc/internal/authz"
	"zoiko.io/schema-registry-svc/internal/compat"
	"zoiko.io/schema-registry-svc/internal/domain"
	"zoiko.io/schema-registry-svc/internal/store"
)

type Handler struct {
	store store.Store
	authz authz.Client
	log   *zap.Logger
}

func New(s store.Store, authzClient authz.Client, log *zap.Logger) *Handler {
	return &Handler{store: s, authz: authzClient, log: log}
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

	// ── Authorization gate (05-security.md §14.6 event-contract mutation
	// rights). Identity is resolved by the gateway (gateway-auth-svc) and
	// arrives as X-Principal-Id / X-Legal-Entity-Id headers. A request with
	// no resolved principal never passed identity verification — fail closed.
	principalID := r.Header.Get("X-Principal-Id")
	legalEntityID := r.Header.Get("X-Legal-Entity-Id")
	correlationID := r.Header.Get("X-Correlation-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, domain.ErrIdentityMissing.Error())
		return
	}
	if err := h.authz.CheckSchemaPublishAllowed(r.Context(), principalID, legalEntityID, correlationID); err != nil {
		switch {
		case errors.Is(err, domain.ErrPublishDenied):
			writeError(w, http.StatusForbidden, domain.ErrPublishDenied.Error())
		default:
			h.log.Error("authorization check failed — failing closed",
				zap.String("event_name", eventName), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, domain.ErrAuthorizationServiceUnavailable.Error())
		}
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

	// 04-data-model.md §17.2: "compatibility mode must be declared". Omitted
	// means BACKWARD — both the safe default and what every previously
	// registered schema was held to, so existing callers are unaffected. An
	// unrecognised mode is refused rather than defaulted: recording a
	// discipline the service does not actually apply would be worse than
	// rejecting the request.
	mode := req.CompatibilityMode
	if mode == "" {
		mode = domain.CompatibilityBackward
	}
	if !domain.ValidCompatibilityMode(mode) {
		writeError(w, http.StatusBadRequest, domain.ErrInvalidCompatibilityMode.Error())
		return
	}

	ctx := r.Context()
	current, err := h.store.LatestVersion(ctx, eventName)
	if err != nil {
		h.log.Error("lookup latest version failed", zap.Error(err), zap.String("event_name", eventName))
		writeError(w, http.StatusServiceUnavailable, domain.ErrStoreUnavailable.Error())
		return
	}

	// currentVersion is both the compatibility baseline and the optimistic
	// concurrency token handed to the store. 0 means "no version yet".
	currentVersion := 0
	if current != nil {
		currentVersion = current.Version

		// NONE is the §17.2 controlled-rollout escape hatch. It is logged at
		// INFO because a contract evolving without a compatibility check is a
		// governance event, not a routine one — the register shows the mode,
		// and the log shows when it was exercised.
		if mode == domain.CompatibilityNone {
			h.log.Info("compatibility check skipped — NONE declared",
				zap.String("event_name", eventName),
				zap.Int("from_version", currentVersion),
				zap.String("principal_id", principalID),
			)
		} else {
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
		}
	}

	newSchema := &domain.EventSchema{
		EventName:         eventName,
		JSONSchema:        req.JSONSchema,
		CompatibilityMode: mode,
		OwningService:     req.OwningService,
		RegisteredBy:      principalID,
		RegisteredAt:      time.Now().UTC(),
	}

	// The version is assigned inside the INSERT, guarded by currentVersion.
	stored, err := h.store.Insert(ctx, newSchema, currentVersion)
	if err != nil {
		if errors.Is(err, domain.ErrVersionRaced) {
			// A concurrent registration won. 409, not 503: nothing is broken,
			// and the caller must re-read and re-check rather than blindly
			// retry — its schema was validated against a version that is no
			// longer latest.
			h.log.Info("schema registration lost a version race",
				zap.String("event_name", eventName),
				zap.Int("checked_against_version", currentVersion),
			)
			writeError(w, http.StatusConflict, domain.ErrVersionRaced.Error())
			return
		}
		h.log.Error("insert schema version failed", zap.Error(err), zap.String("event_name", eventName))
		writeError(w, http.StatusServiceUnavailable, domain.ErrStoreUnavailable.Error())
		return
	}

	h.log.Info("schema version registered",
		zap.String("event_name", eventName),
		zap.Int("version", stored.Version),
		zap.String("compatibility_mode", stored.CompatibilityMode),
	)
	writeJSON(w, http.StatusCreated, stored)
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
