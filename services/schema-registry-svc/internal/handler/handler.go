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

	// platformScopeID authorizes registrations that carry no legal entity.
	// See config.AuthZPlatformScopeID for why an empty scope cannot simply be
	// passed through.
	platformScopeID string
}

func New(s store.Store, authzClient authz.Client, platformScopeID string, log *zap.Logger) *Handler {
	return &Handler{store: s, authz: authzClient, platformScopeID: platformScopeID, log: log}
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
	//
	// This runs BEFORE the request is validated, deliberately. An
	// unauthenticated caller should not be able to learn what this service
	// considers a well-formed event name or a well-formed schema by watching
	// which 400s come back — the first thing a caller must establish is who
	// they are, and only then does the service start discussing their input.
	principalID := r.Header.Get("X-Principal-Id")
	correlationID := r.Header.Get("X-Correlation-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, domain.ErrIdentityMissing.Error())
		return
	}

	// An event contract belongs to the platform, not to a legal entity. The
	// header used to be passed through verbatim, and authorization-svc rejects
	// an empty legal_entity_id outright — so a caller that sent no entity got
	// a non-200 from authorization-svc, which this client reports as
	// "authorization service unavailable": a 503 blaming infrastructure for a
	// scope the request was never going to carry.
	scopeID := r.Header.Get("X-Legal-Entity-Id")
	if scopeID == "" {
		scopeID = h.platformScopeID
	}

	if err := h.authz.CheckSchemaPublishAllowed(r.Context(), principalID, scopeID, correlationID); err != nil {
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

	// The name is this registry's primary key and is echoed back in every
	// response. It used to be accepted verbatim, so the key of a canonical
	// registry was a free-text field, and a name longer than the column died
	// in Postgres as a 503.
	if !domain.ValidEventName(eventName) {
		writeError(w, http.StatusBadRequest, domain.ErrEventNameInvalid.Error())
		return
	}

	var req domain.RegisterSchemaRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// One validator for "is this something the registry can hold as a
	// contract", shared with the compatibility checker's own parse. It used to
	// be `json.Valid` alone, which passes `123` and `null` — see
	// domain.ValidateJSONSchema for why storing one of those permanently
	// bricks the event it was registered under.
	if err := domain.ValidateJSONSchema(req.JSONSchema); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.OwningService) > domain.MaxOwningServiceLen {
		writeError(w, http.StatusBadRequest, domain.ErrOwningServiceTooLong.Error())
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
	if !h.requireIdentity(w, r) {
		return
	}
	eventName := chi.URLParam(r, "eventName")
	if !domain.ValidEventName(eventName) {
		// A name that cannot be registered names no contract, so it reads as
		// not-found rather than as a validation error on a read.
		writeError(w, http.StatusNotFound, domain.ErrEventNotFound.Error())
		return
	}
	schema, err := h.store.LatestVersion(r.Context(), eventName)
	h.respondOne(w, schema, err)
}

// ── GET /v1/schemas/{eventName}/versions/{version} ──────────────────────────

func (h *Handler) GetVersion(w http.ResponseWriter, r *http.Request) {
	if !h.requireIdentity(w, r) {
		return
	}
	eventName := chi.URLParam(r, "eventName")
	if !domain.ValidEventName(eventName) {
		writeError(w, http.StatusNotFound, domain.ErrEventNotFound.Error())
		return
	}
	version, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "version must be an integer")
		return
	}
	// Versions start at 1 and are assigned by the registry. A zero or negative
	// one is a caller mistake, not a lookup — it used to reach Postgres as a
	// perfectly valid comparison that matched nothing, so it answered 404 and
	// read as "that version was deleted".
	if version < 1 {
		writeError(w, http.StatusBadRequest, "version must be 1 or greater")
		return
	}
	schema, err := h.store.Version(r.Context(), eventName, version)
	h.respondOne(w, schema, err)
}

func (h *Handler) respondOne(w http.ResponseWriter, schema *domain.EventSchema, err error) {
	if err != nil {
		h.writeStoreErr(w, "lookup schema failed", err)
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
	if !h.requireIdentity(w, r) {
		return
	}
	eventName := chi.URLParam(r, "eventName")
	if !domain.ValidEventName(eventName) {
		writeError(w, http.StatusNotFound, domain.ErrEventNotFound.Error())
		return
	}
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	versions, err := h.store.Versions(r.Context(), eventName, limit, offset)
	if err != nil {
		h.writeStoreErr(w, "list versions failed", err)
		return
	}
	// An empty page beyond the end of a real event's history is not a missing
	// event. Only offset 0 can distinguish the two.
	if len(versions) == 0 && offset == 0 {
		writeError(w, http.StatusNotFound, domain.ErrEventNotFound.Error())
		return
	}
	if versions == nil {
		versions = []*domain.EventSchema{}
	}
	writeJSON(w, http.StatusOK, versions)
}

// ── GET /v1/schemas ──────────────────────────────────────────────────────────

func (h *Handler) ListEventNames(w http.ResponseWriter, r *http.Request) {
	if !h.requireIdentity(w, r) {
		return
	}
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	names, err := h.store.EventNames(r.Context(), limit, offset)
	if err != nil {
		h.writeStoreErr(w, "list event names failed", err)
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, names)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// requireIdentity refuses a read from a caller the gateway never identified.
//
// Every read used to be open: anything that could reach the port could
// enumerate the platform's entire event-contract catalogue — every event name,
// every payload field, and which service owns it. That is a map of the
// platform's internals, and 05-security.md §14.6 names schema-registry ACCESS,
// not only mutation, as the thing to protect.
//
// Deliberately identity only, with no per-entity authorization: an event
// contract is platform-wide reference data with no legal entity of its own, so
// a grant scoped to one entity would answer a question the data does not have.
// The bar is "the gateway verified who you are", which is the same bar
// board-resolutions-svc applies to its reads.
func (h *Handler) requireIdentity(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Principal-Id") == "" {
		writeError(w, http.StatusUnauthorized, domain.ErrIdentityMissing.Error())
		return false
	}
	return true
}

// writeStoreErr maps a store failure to the status it deserves. Everything
// used to answer 503 "schema store unavailable", including a caller's
// over-long field, which reported an outage for a validation problem.
func (h *Handler) writeStoreErr(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, domain.ErrFieldTooLong) {
		writeError(w, http.StatusBadRequest, domain.ErrFieldTooLong.Error())
		return
	}
	h.log.Error(what, zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, domain.ErrStoreUnavailable.Error())
}

// maxRequestBytes bounds a registration body. json_schema is JSONB with no
// width limit, so without a cap one request could stream unbounded memory into
// the decoder before any validation ran.
const maxRequestBytes = 1 << 20 // 1 MiB

// decodeJSON reads a JSON request body with a size cap and no tolerance for
// unknown fields — `{"json_schemas": ...}` used to be discarded silently and
// answer "json_schema is required" for a field the caller believed they had
// sent, and `{"compatibility_mode_": "NONE"}` would have been accepted as
// BACKWARD, recording a discipline the caller did not ask for.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds 1 MiB")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

const (
	defaultLimit = 100
	maxLimit     = 500
)

// parsePaging bounds a register read. Both lists were unbounded — every event
// name, and every version of an event, forever — and a discarded strconv error
// is the platform's recurring shape: limit=abc silently defaulted, offset=-1
// reached Postgres and answered 503.
func parsePaging(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset = defaultLimit, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest,
				"limit must be an integer between 1 and "+strconv.Itoa(maxLimit))
			return 0, 0, false
		}
		limit = n
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

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
