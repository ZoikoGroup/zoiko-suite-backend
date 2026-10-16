package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/retention"
)

// WFP-NP-041: the disposition guard. A service that owns WFP records asks here before it deletes, redacts,
// exports or migrates them. The answer comes from retention-registry-svc (legal holds and retention policy);
// WFP adds only the rule that every missing fact means "not allowed". Nothing here deletes anything.

const actionDispositionCheck = "WFP_DISPOSITION_CHECK"

// RetentionResolver is the read-only view of retention-registry-svc the guard needs.
type RetentionResolver interface {
	Resolve(ctx context.Context, caller retention.Caller, recordClass, jurisdictionCode, entityRef string) (*retention.Resolution, error)
}

type WFPDispositionHandler struct {
	retention RetentionResolver
	authz     AuthZClient
	now       func() time.Time
	log       *zap.Logger
}

func NewWFPDisposition(rr RetentionResolver, authz AuthZClient, log *zap.Logger) *WFPDispositionHandler {
	return &WFPDispositionHandler{retention: rr, authz: authz, now: time.Now, log: log}
}

func RegisterWFPDispositionRoutes(r chi.Router, h *WFPDispositionHandler) {
	r.Get("/v1/payroll/wfp/disposition-check", h.Check)
}

// Check answers whether a record class (optionally one entity's records) may be disposed of right now.
//
//	GET /v1/payroll/wfp/disposition-check?record_class=WFP_PAYROLL_RESULT&record_created_at=2019-03-01
//	    [&entity_ref=...][&jurisdiction_code=GB]
//
// record_created_at is the creation date of the oldest record in question; without it the retention period
// cannot be judged and the answer is not allowed.
func (h *WFPDispositionHandler) Check(w http.ResponseWriter, r *http.Request) {
	principal := r.Header.Get("X-Principal-Id")
	tenant := svcmiddleware.TenantFromContext(r.Context())
	if principal == "" || tenant == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return
	}
	entity := scopeEntity(r)
	if entity == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "X-Legal-Entity-Id is required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, entity, actionDispositionCheck); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "forbidden", err.Error())
		} else {
			writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
		}
		return
	}
	q := r.URL.Query()
	class := strings.TrimSpace(q.Get("record_class"))
	if !domain.ValidRecordClasses[class] {
		writeError(w, http.StatusBadRequest, "validation_failed", "record_class must be a WFP record class")
		return
	}
	var created *time.Time
	if s := strings.TrimSpace(q.Get("record_created_at")); s != "" {
		t, err := parseWhen(s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", "record_created_at must be YYYY-MM-DD or RFC 3339")
			return
		}
		created = &t
	}
	entityRef := strings.TrimSpace(q.Get("entity_ref"))
	if entityRef == "" {
		entityRef = entity
	}

	res, err := h.retention.Resolve(r.Context(), retention.Caller{TenantID: tenant, PrincipalID: principal, CorrelationID: getCorrelationID(r)},
		class, strings.TrimSpace(q.Get("jurisdiction_code")), entityRef)
	if err != nil {
		h.log.Error("retention registry could not answer the disposition check", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error_code": "retention_unavailable", "error_message": err.Error(), "allowed": false,
		})
		return
	}
	var holdID, policyID *string
	var minDays *int
	if res.Hold != nil {
		holdID = &res.Hold.ID
	}
	if res.Policy != nil {
		policyID, minDays = &res.Policy.ID, &res.Policy.MinRetentionDays
	}
	writeJSON(w, http.StatusOK, domain.DecideDisposition(class, res.Blocked, holdID, policyID, minDays, created, h.now()))
}

func parseWhen(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}
