// Package handler provides HTTP endpoints for audit-event-store-svc,
// including AUD-10 archive/verify capabilities and Doc 03 §14.1 query API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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

// requirePrincipal establishes the caller's principal from X-Principal-Id and
// refuses the request (401) if it is absent.
//
// Only the principal identity counts. envelope.Actor() would also return the
// workload id (X-Workload-Id) when no principal is present, which is right for
// service-to-service plumbing elsewhere in the platform but is NOT what this
// service's contract says: every route requires X-Principal-Id, and an
// authorization grant is checked for that principal. A request carrying only a
// workload id therefore gets 401 here instead of being authorized as if the
// workload id were a principal.
//
// The envelope's parsed actor_subject_id is the same X-Principal-Id header
// (svcenvelope.Middleware in the real server); the direct header read covers the
// unit tests, which mount a bare router with no envelope middleware.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	e, _ := envelope.FromContext(r.Context())
	principalID := e.ActorSubjectID
	if principalID == "" {
		principalID = r.Header.Get(envelope.HeaderActorSubjectID)
	}
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "principal_required", "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// requireTenant resolves the caller's tenant from the already-validated
// envelope (X-Tenant-Id, set by gateway-auth-svc from the signed identity
// envelope) and refuses the request when it is absent.
//
// Tenant scope is mandatory on every route — the audit doc's §2 "Segregation
// of Duties & Auth" states "X-Tenant-Id mandatory on all queries (401 if
// missing)". A missing tenant is an authentication failure, not a validation
// error (INV-01: "Every authenticated operation resolves exactly one tenant
// context before data access"), so this is 401 — matching the canonical
// envelope's own StatusFor — not the 400 this service previously returned.
// The tenant is taken only from server-established sources (envelope, then the
// header); a caller-supplied query parameter is not treated as tenant authority.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	e, _ := envelope.FromContext(r.Context())
	tenantID := e.TenantID
	if tenantID == "" {
		tenantID = r.Header.Get(envelope.HeaderTenantID)
	}
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_required", "X-Tenant-Id header is required")
		return "", false
	}
	return tenantID, true
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	params, perr := buildQueryParams(tenantID, r.URL.Query())
	if perr != nil {
		writeError(w, http.StatusBadRequest, perr.code, perr.message)
		return
	}

	result, err := h.store.QueryEvents(ctx, params)
	if err != nil {
		h.log.Error("failed to query audit events", zap.Error(err), zap.String("tenant_id", tenantID))
		writeError(w, http.StatusInternalServerError, "query_failed", "failed to query audit events")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// queryParamError is a malformed GET /v1/events query parameter. It is reported
// as 400 — a malformed filter is never silently dropped, because that would
// answer a narrower question with a broader (unfiltered) result.
type queryParamError struct {
	code    string
	message string
}

// maxQueryLimit is the page-size ceiling (enforced again in the store). A limit
// above it is clamped, not rejected; the documented maximum page is 200 events.
const maxQueryLimit = 200

// buildQueryParams parses and validates the GET /v1/events query string.
// Accepted: actor|principal_id, entity|legal_entity_id, action|event_type,
// workflow|correlation_id, from, to (RFC 3339), limit (integer >= 1), offset
// (integer >= 0). An empty value is the same as an absent one.
func buildQueryParams(tenantID string, q url.Values) (domain.QueryEventsParams, *queryParamError) {
	params := domain.QueryEventsParams{
		TenantID:      tenantID,
		LegalEntityID: firstNonEmpty(q.Get("entity"), q.Get("legal_entity_id")),
		PrincipalID:   firstNonEmpty(q.Get("actor"), q.Get("principal_id")),
		EventType:     firstNonEmpty(q.Get("action"), q.Get("event_type")),
		CorrelationID: firstNonEmpty(q.Get("workflow"), q.Get("correlation_id")),
		Limit:         50,
		Offset:        0,
	}

	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return params, &queryParamError{"invalid_limit", "limit must be an integer >= 1"}
		}
		if n > maxQueryLimit {
			n = maxQueryLimit
		}
		params.Limit = n
	}
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return params, &queryParamError{"invalid_offset", "offset must be an integer >= 0"}
		}
		params.Offset = n
	}
	if raw := q.Get("from"); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return params, &queryParamError{"invalid_from", "from must be an RFC 3339 timestamp, e.g. 2026-01-02T15:04:05Z"}
		}
		params.FromTime = &ts
	}
	if raw := q.Get("to"); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return params, &queryParamError{"invalid_to", "to must be an RFC 3339 timestamp, e.g. 2026-01-02T15:04:05Z"}
		}
		params.ToTime = &ts
	}
	if params.FromTime != nil && params.ToTime != nil && params.FromTime.After(*params.ToTime) {
		return params, &queryParamError{"invalid_range", "from must not be after to"}
	}
	return params, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
	if _, ok := h.requireTenant(w, r); !ok {
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
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
		TenantID:             tenantID,
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	a, err := h.store.GetArchive(r.Context(), chi.URLParam(r, "id"), tenantID)
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	v, err := h.store.VerifyArchive(r.Context(), domain.VerifyArchiveParams{
		ArchiveID:             chi.URLParam(r, "id"),
		TenantID:              tenantID,
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListVerifications(r.Context(), chi.URLParam(r, "id"), tenantID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrTenantRequired):
		// INV-01: missing tenant context is an authentication failure (401),
		// consistent with requireTenant.
		writeError(w, http.StatusUnauthorized, "tenant_required", err.Error())
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
