// Package handler provides HTTP endpoints for audit-event-store-svc,
// including AUD-10 archive/verify capabilities and Doc 03 §14.1 query API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/audit-event-store-svc/internal/authz"
	"zoiko.io/audit-event-store-svc/internal/domain"
	"zoiko.io/audit-event-store-svc/internal/envelope"
	"zoiko.io/audit-event-store-svc/internal/store"
)

// Store combines archive management and event querying.
type Store interface {
	store.ArchiveStore
	store.EventQueryStore
}

// AuthzChecker is the subset of authz.Client this handler depends on,
// narrowed to an interface so tests can substitute a stub.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// platformScopeID is the legal_entity_id authorization-svc checks are made
// against for this service's actions. None of this service's permissions
// are legal-entity-scoped: the audit ledger's own SoD model (see the audit
// doc's §2 "Audit Log Archival" row) assigns them to platform-level roles —
// Compliance Auditor, System Admin — not per-tenant business users, so the
// platform sentinel is correct here rather than a per-request tenant_id.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

const (
	// AuditEventRead gates GET /v1/events and POST /v1/events/verify.
	// Added because listEvents trusted a caller-declared X-Tenant-Id/
	// ?tenant_id= with no check that the caller may see that tenant's
	// evidence — any unauthenticated caller could read any tenant's full
	// audit trail. Named to parallel the _READ convention already used in
	// sibling services (e.g. ai-governance-svc's PolicyChangeRead).
	AuditEventRead = "AUDIT_EVENT_READ"

	// AuditArchiveManage gates the four /v1/archives routes. The audit's
	// own SoD table lumps archive creation, retrieval, and verification
	// under one "Audit Log Archival" duty, so one action constant covers
	// all four rather than inventing a different one per verb.
	AuditArchiveManage = "AUDIT_ARCHIVE_MANAGE"
)

type Handler struct {
	store Store
	authz AuthzChecker
	log   *zap.Logger
}

func New(s Store, az AuthzChecker, log *zap.Logger) *Handler {
	return &Handler{store: s, authz: az, log: log}
}

// requirePrincipal reads the caller's identity from the already-validated
// envelope (X-Principal-Id, checked for presence by svcenvelope.Middleware
// upstream) and refuses the request if it is absent.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	e, _ := envelope.FromContext(r.Context())
	principalID := e.Actor()
	if principalID == "" {
		// Mirrors listEvents' own tenantID fallback: envelope context first
		// (set by svcenvelope.Middleware in the real server), direct header
		// read otherwise (unit tests mount a bare router with no envelope
		// middleware, same as every existing test in this package).
		principalID = r.Header.Get(envelope.HeaderActorSubjectID)
	}
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "principal_required", "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// authorize checks principalID against authorization-svc for actionType,
// failing closed (503) if authorization-svc itself is unreachable rather
// than admitting the request — an evidence ledger must never silently
// open up because its authorization dependency is down.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, platformScopeID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "forbidden", "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", "authorization service unavailable")
		}
		return false
	}
	return true
}

// RegisterRoutes mounts the service's HTTP routes on r.
func RegisterRoutes(r chi.Router, h *Handler) {
	// Doc 03 §14.1 Event Query API and Live Hash Chain Verification
	r.Get("/v1/events", h.listEvents)
	r.Post("/v1/events/verify", h.verifyEventsChain)

	// AUD-10 archive/verify routes
	r.Post("/v1/archives", h.createArchive)
	r.Get("/v1/archives/{id}", h.getArchive)
	r.Post("/v1/archives/{id}/verify", h.verifyArchive)
	r.Get("/v1/archives/{id}/verifications", h.listVerifications)
}

func (h *Handler) listEvents(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, AuditEventRead) {
		return
	}

	ctx := r.Context()
	e, _ := envelope.FromContext(ctx)

	tenantID := e.TenantID
	if tenantID == "" {
		tenantID = r.Header.Get("X-Tenant-Id")
	}
	if tenantID == "" {
		tenantID = r.URL.Query().Get("tenant_id")
	}
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant_required", "X-Tenant-Id header is required")
		return
	}

	q := r.URL.Query()
	params := domain.QueryEventsParams{
		TenantID:      tenantID,
		LegalEntityID: q.Get("entity"),
		PrincipalID:   q.Get("actor"),
		EventType:     q.Get("action"),
		CorrelationID: q.Get("workflow"),
		Limit:         50,
		Offset:        0,
	}

	if params.LegalEntityID == "" {
		params.LegalEntityID = q.Get("legal_entity_id")
	}
	if params.PrincipalID == "" {
		params.PrincipalID = q.Get("principal_id")
	}
	if params.EventType == "" {
		params.EventType = q.Get("event_type")
	}
	if params.CorrelationID == "" {
		params.CorrelationID = q.Get("correlation_id")
	}

	if limitStr := q.Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			params.Limit = l
		}
	}
	if offsetStr := q.Get("offset"); offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
			params.Offset = o
		}
	}

	if fromStr := q.Get("from"); fromStr != "" {
		if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
			params.FromTime = &t
		}
	}
	if toStr := q.Get("to"); toStr != "" {
		if t, err := time.Parse(time.RFC3339, toStr); err == nil {
			params.ToTime = &t
		}
	}

	result, err := h.store.QueryEvents(ctx, params)
	if err != nil {
		h.log.Error("failed to query audit events", zap.Error(err), zap.String("tenant_id", tenantID))
		writeError(w, http.StatusInternalServerError, "query_failed", "failed to query audit events")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

type verifyChainResponse struct {
	Verified      bool   `json:"verified"`
	CheckedEvents int64  `json:"checkedEvents"`
	CheckedCount  int64  `json:"checked_events"`
	Timestamp     string `json:"timestamp"`
}

func (h *Handler) verifyEventsChain(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, AuditEventRead) {
		return
	}

	verified, count, err := h.store.VerifyChain(r.Context())
	if err != nil {
		h.log.Error("failed to verify events chain", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "verify_failed", "failed to verify chain")
		return
	}
	writeJSON(w, http.StatusOK, verifyChainResponse{
		Verified:      verified,
		CheckedEvents: count,
		CheckedCount:  count,
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
	})
}

type createArchiveRequest struct {
	FromSequence int64 `json:"from_sequence"`
	ToSequence   int64 `json:"to_sequence"`
}

func (h *Handler) createArchive(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, AuditArchiveManage) {
		return
	}

	var req createArchiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	a, err := h.store.CreateArchive(r.Context(), domain.CreateArchiveParams{
		FromSequence:         req.FromSequence,
		ToSequence:           req.ToSequence,
		CreatedByPrincipalID: principalID,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (h *Handler) getArchive(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, AuditArchiveManage) {
		return
	}

	a, err := h.store.GetArchive(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) verifyArchive(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, AuditArchiveManage) {
		return
	}

	v, err := h.store.VerifyArchive(r.Context(), domain.VerifyArchiveParams{
		ArchiveID:             chi.URLParam(r, "id"),
		VerifiedByPrincipalID: principalID,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) listVerifications(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, AuditArchiveManage) {
		return
	}

	list, err := h.store.ListVerifications(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrArchiveNotFound):
		writeError(w, http.StatusNotFound, "archive_not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidRange):
		writeError(w, http.StatusBadRequest, "invalid_range", err.Error())
	case errors.Is(err, domain.ErrEmptyRange):
		writeError(w, http.StatusBadRequest, "empty_range", err.Error())
	case errors.Is(err, domain.ErrChainBroken):
		writeError(w, http.StatusConflict, "chain_broken", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}
