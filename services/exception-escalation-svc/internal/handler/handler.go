package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/exception-escalation-svc/internal/authz"
	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/middleware"
	"zoiko.io/exception-escalation-svc/internal/store"
)

// AuthZClient is the interface the handler depends on to authorize write
// actions against the Governance Plane authorization-svc. Every write action
// must be checked before it is performed and must fail closed.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

const (
	actionExceptionCreate   = "EXCEPTION_CREATE"
	actionExceptionEscalate = "EXCEPTION_ESCALATE"
	actionExceptionResolve  = "EXCEPTION_RESOLVE"

	// Read actions. Every GET in this service used to be served to any caller
	// in the tenant without an authorization decision; each now requires a
	// principal holding the read action of its resource family.
	actionExceptionRead    = "EXCEPTION_READ"
	actionTaskRead         = "TASK_READ"
	actionDeadlineRead     = "DEADLINE_READ"
	actionAuditFindingRead = "AUDIT_FINDING_READ"
)

// Stable error codes for refusals of the actor and segregation-of-duties
// rules ("error" keeps its human-readable message; "code" is additive).
const (
	codeActorMismatch       = "actor_mismatch"
	codeSegregationOfDuties = "segregation_of_duties"
)

// Handler serves the exception-escalation API. Events are not published from
// here: every write emits its event through the store's transactional
// outbox, delivered by internal/outbox.Relay.
type Handler struct {
	store  store.Store
	authz  AuthZClient
	logger *zap.Logger
}

func New(st store.Store, az AuthZClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, logger: logger}
}

// withEventMeta stamps the request identity (gateway-verified X-Principal-Id
// and X-Correlation-ID) on the context, where the store reads it for the
// outbox events a write emits.
func withEventMeta(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := store.WithEventMeta(r.Context(), store.EventMeta{
			ActorID:       r.Header.Get("X-Principal-Id"),
			CorrelationID: r.Header.Get("X-Correlation-ID"),
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authorizeRead requires a principal (401) holding action for scope (403 on
// deny, 503 when authorization-svc is unreachable). Pattern:
// purchase-request-svc authorizeRead.
func (h *Handler) authorizeRead(w http.ResponseWriter, r *http.Request, scope, action string) bool {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, scope, action); err != nil {
		h.writeAuthzErr(w, err)
		return false
	}
	return true
}

// resolveActor applies the platform actor rule to a body-supplied actor
// field: absent -> the principal; different -> 403 actor_mismatch.
func resolveActor(w http.ResponseWriter, field, claimed, principalID string) (string, bool) {
	if claimed != "" && claimed != principalID {
		writeCoded(w, http.StatusForbidden, codeActorMismatch, field+" must match the calling principal")
		return "", false
	}
	return principalID, true
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/exception-escalation", func(r chi.Router) {
		r.Use(withEventMeta)
		r.Post("/exceptions", h.CreateException)
		r.Get("/exceptions", h.ListExceptions)
		r.Get("/exceptions/{id}", h.GetExceptionByID)
		r.Post("/exceptions/{id}/escalate", h.EscalateException)
		r.Post("/exceptions/{id}/resolve", h.ResolveException)

		r.Get("/escalations", h.ListEscalations)
	})
}

func (h *Handler) CreateException(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateExceptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.ExceptionType == "" || req.LinkedObjectType == "" || req.LinkedObjectID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, exception_type, linked_object_type, and linked_object_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// The creator is the authenticated caller. A body value is only an echo:
	// trusting it let a caller omit or forge created_by and then pass the
	// resolve-time self-approval check on their own case.
	if _, ok := resolveActor(w, "created_by", req.CreatedBy, principalID); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionExceptionCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	c := &domain.ExceptionCase{
		TenantID:         tenantID,
		LegalEntityID:    req.LegalEntityID,
		JurisdictionID:   req.JurisdictionID,
		ExceptionType:    req.ExceptionType,
		SeverityLevel:    req.SeverityLevel,
		LinkedObjectType: req.LinkedObjectType,
		LinkedObjectID:   req.LinkedObjectID,
		Description:      req.Description,
		AssignedToRole:   req.AssignedToRole,
		CreatedBy:        principalID,
	}

	if err := h.store.CreateException(r.Context(), c); err != nil {
		h.logger.Error("create exception case failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create exception case")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) GetExceptionByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	c, err := h.store.GetExceptionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrExceptionCaseNotFound) {
			writeError(w, http.StatusNotFound, "exception case not found")
			return
		}
		h.logger.Error("get exception case failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to get exception case")
		return
	}
	if !h.authorizeRead(w, r, c.LegalEntityID, actionExceptionRead) {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ListExceptions(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	caseStatus := r.URL.Query().Get("case_status")
	severityLevel := r.URL.Query().Get("severity_level")
	exceptionType := r.URL.Query().Get("exception_type")

	// Authorized against the requested legal entity, or the tenant when the
	// list spans entities (purchase-request-svc ListRequests).
	scope := legalEntityID
	if scope == "" {
		scope = middleware.GetTenantID(r.Context())
	}
	if !h.authorizeRead(w, r, scope, actionExceptionRead) {
		return
	}

	cases, err := h.store.ListExceptions(r.Context(), legalEntityID, caseStatus, severityLevel, exceptionType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list exception cases")
		return
	}
	if cases == nil {
		cases = []domain.ExceptionCase{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"exception_cases": cases,
		"total":           len(cases),
	})
}

func (h *Handler) EscalateException(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.EscalateCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.EscalatedToRole == "" || req.EscalationReason == "" {
		writeError(w, http.StatusBadRequest, "escalated_to_role and escalation_reason are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existingCase, err := h.store.GetExceptionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrExceptionCaseNotFound) {
			writeError(w, http.StatusNotFound, "exception case not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get exception case")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existingCase.LegalEntityID, actionExceptionEscalate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	escalatedBy, ok := resolveActor(w, "escalated_by", req.EscalatedBy, principalID)
	if !ok {
		return
	}
	req.EscalatedBy = escalatedBy

	escRecord, updatedCase, err := h.store.EscalateException(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrExceptionCaseNotFound):
			writeError(w, http.StatusNotFound, "exception case not found")
		case errors.Is(err, domain.ErrCaseAlreadyClosed):
			writeError(w, http.StatusConflict, "exception case is already closed")
		default:
			h.logger.Error("escalate exception case failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "failed to escalate exception case")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"escalation_record": escRecord,
		"exception_case":    updatedCase,
	})
}

func (h *Handler) ResolveException(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.ResolveCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// closed_by is no longer required in the body: the closer is the
	// authenticated principal (a differing value is refused below).

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existingCase, err := h.store.GetExceptionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrExceptionCaseNotFound) {
			writeError(w, http.StatusNotFound, "exception case not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get exception case")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existingCase.LegalEntityID, actionExceptionResolve); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Segregation of Duties (docs/original_doc/zoiko_suite_doc1.txt §12.3):
	// the principal who created the exception case may not be the same
	// principal who resolves and closes it.
	if existingCase.CreatedBy == principalID {
		writeCoded(w, http.StatusForbidden, codeSegregationOfDuties, domain.ErrSelfApprovalNotAllowed.Error())
		return
	}
	closedBy, ok := resolveActor(w, "closed_by", req.ClosedBy, principalID)
	if !ok {
		return
	}
	req.ClosedBy = closedBy

	updatedCase, err := h.store.ResolveException(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrExceptionCaseNotFound):
			writeError(w, http.StatusNotFound, "exception case not found")
		case errors.Is(err, domain.ErrCaseAlreadyClosed):
			writeError(w, http.StatusConflict, "exception case is already closed")
		case errors.Is(err, domain.ErrSelfApprovalNotAllowed):
			// Store re-check under the row lock.
			writeCoded(w, http.StatusForbidden, codeSegregationOfDuties, domain.ErrSelfApprovalNotAllowed.Error())
		default:
			h.logger.Error("resolve exception case failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "failed to resolve exception case")
		}
		return
	}
	writeJSON(w, http.StatusOK, updatedCase)
}

func (h *Handler) ListEscalations(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("escalated_to_role")
	status := r.URL.Query().Get("escalation_status")

	// Escalation records carry no legal entity; the list is tenant-wide, so
	// it is authorized at tenant scope.
	if !h.authorizeRead(w, r, middleware.GetTenantID(r.Context()), actionExceptionRead) {
		return
	}

	escalations, err := h.store.ListEscalations(r.Context(), role, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list escalations")
		return
	}
	if escalations == nil {
		escalations = []domain.EscalationRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"escalation_records": escalations,
		"total":              len(escalations),
	})
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "authorization denied")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeCoded is writeError plus a stable machine-readable code.
func writeCoded(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}
