package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/retention-registry-svc/internal/authz"
	"zoiko.io/retention-registry-svc/internal/domain"
	"zoiko.io/retention-registry-svc/internal/events"
	svcmiddleware "zoiko.io/retention-registry-svc/internal/middleware"
	"zoiko.io/retention-registry-svc/internal/store"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

const (
	RetentionPolicyCreate = "RETENTION_POLICY_CREATE"
	LegalHoldCreate       = "LEGAL_HOLD_CREATE"
	LegalHoldRelease      = "LEGAL_HOLD_RELEASE"
	LegalHoldRead         = "LEGAL_HOLD_READ"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthzChecker
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// resolveTenantScope returns the tenant dimension for the resolve read,
// taken from the verified X-Tenant-Id rather than the caller's query
// string.
//
// Resolve used to read ?tenant_id= directly with no authorization, so any
// caller could ask what retention rules and — more sensitively — what legal
// holds applied to any tenant's record class. A hold is evidence of
// litigation or investigation.
//
// A NIL return is legitimate and is NOT the fail-closed case, which makes
// this service behave like kill-switch-registry-svc rather than
// evidence-manifest-svc. A nil tenant means "the platform-level question",
// and under migration 000002's policy a caller with no verified tenant
// still sees every tenant_id IS NULL row — platform-wide retention rules
// and platform-wide holds — and nobody's tenant-specific ones. Hiding those
// is precisely the failure that would permit deleting records under a
// platform-wide hold, so middleware.TenantContext stays permissive here.
//
// A ?tenant_id= that disagrees with the verified header is refused rather
// than ignored.
func (h *Handler) resolveTenantScope(w http.ResponseWriter, r *http.Request, declared string) (*string, bool) {
	verified := svcmiddleware.TenantFromContext(r.Context())
	if declared != "" && declared != verified {
		writeError(w, http.StatusForbidden,
			"tenant_id does not match the verified X-Tenant-Id")
		return nil, false
	}
	return strPtrOrNil(verified), true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, tenantID, actionType string) bool {
	scope := platformScopeID
	if tenantID != "" {
		scope = tenantID
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, scope, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		}
		return false
	}
	return true
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/retention-policies", func(r chi.Router) {
		r.Post("/", h.CreateRetentionPolicy)
		r.Get("/", h.ListRetentionPolicies)
	})
	r.Route("/v1/legal-holds", func(r chi.Router) {
		r.Post("/", h.CreateLegalHold)
		r.Get("/", h.ListLegalHolds)
		r.Get("/{id}", h.GetLegalHold)
		r.Post("/{id}/release", h.ReleaseLegalHold)
	})
	r.Get("/v1/retention/resolve", h.Resolve)
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// CreateRetentionPolicy handles POST /v1/retention-policies.
func (h *Handler) CreateRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRetentionPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RecordClass == "" || req.LegalRegulatoryBasis == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "record_class, legal_regulatory_basis, and effective_from are required")
		return
	}
	if req.MinRetentionDays <= 0 {
		writeError(w, http.StatusBadRequest, "min_retention_days must be positive")
		return
	}
	effectiveFrom, err := time.Parse(time.RFC3339, req.EffectiveFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "effective_from must be RFC3339")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.TenantID, RetentionPolicyCreate) {
		return
	}

	p := &domain.RetentionPolicy{
		RetentionPolicyID:    uuid.NewString(),
		RecordClass:          req.RecordClass,
		JurisdictionCode:     strPtrOrNil(req.JurisdictionCode),
		TenantID:             strPtrOrNil(req.TenantID),
		MinRetentionDays:     req.MinRetentionDays,
		MaxRetentionDays:     req.MaxRetentionDays,
		LegalRegulatoryBasis: req.LegalRegulatoryBasis,
		SourceRightsBasis:    strPtrOrNil(req.SourceRightsBasis),
		PrivacyBasis:         strPtrOrNil(req.PrivacyBasis),
		PolicyStatus:         "ACTIVE",
		EffectiveFrom:        effectiveFrom,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateRetentionPolicy(r.Context(), p); err != nil {
		h.logger.Error("create retention policy failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create retention policy")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "retention_policy.created", EntityID: p.RetentionPolicyID, TenantID: req.TenantID,
		Jurisdiction: req.JurisdictionCode, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: p,
	})
	writeJSON(w, http.StatusCreated, p)
}

// CreateLegalHold handles POST /v1/legal-holds.
func (h *Handler) CreateLegalHold(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateLegalHoldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ScopeDescription == "" || req.Authority == "" {
		writeError(w, http.StatusBadRequest, "scope_description and authority are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.TenantID, LegalHoldCreate) {
		return
	}

	now := time.Now().UTC()
	hld := &domain.LegalHold{
		LegalHoldID:          uuid.NewString(),
		ScopeDescription:     req.ScopeDescription,
		CustodiansObjects:    req.CustodiansObjects,
		Authority:            req.Authority,
		RecordClass:          strPtrOrNil(req.RecordClass),
		TenantID:             strPtrOrNil(req.TenantID),
		EntityRef:            strPtrOrNil(req.EntityRef),
		HoldStatus:           "ACTIVE",
		StartedAt:            now,
		CreatedAt:            now,
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateLegalHold(r.Context(), hld); err != nil {
		h.logger.Error("create legal hold failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create legal hold")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "legal_hold.engaged", EntityID: hld.LegalHoldID, TenantID: req.TenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: hld,
	})
	writeJSON(w, http.StatusCreated, hld)
}

// GetLegalHold handles GET /v1/legal-holds/{id}.
//
// THIS ROUTE HAD NO IDENTITY CHECK OF ANY KIND. It called
// FindLegalHoldByID(id) with no principal, no tenant, and no authorization —
// so anything that could reach the port could read any hold in any tenant by
// id, and a hold names the court or regulator that ordered the freeze, the
// matter, and the custodians holding the evidence. Reading it now requires a
// verified principal and is scoped to the caller's tenant.
//
// No authz action is checked beyond the tenant scope, matching the create/list
// posture: a hold in your own tenant is visible to your own principals, and it
// is the RELEASE that is privileged. Widening that to a per-read grant is a
// defensible position, but it is a policy change rather than a defect fix and
// would need the grant seeding to change with it.
//
// BOTH GATES ARE NOW PRESENT. The read is scoped to the caller's verified
// tenant in the store, and it is additionally authorized against LEGAL_HOLD_READ
// for the hold's own tenant. The tenant scope answers "whose hold is this"; the
// action grant answers "may this principal read holds at all". Fetch-then-
// authorize, in that order and matching ReleaseLegalHold: the store already
// refuses another tenant's hold with the same ErrLegalHoldNotFound a genuinely
// absent id produces, so authorizing against a caller-supplied scope first would
// let a caller nominate an entity they hold a grant for and probe for holds
// outside it.
func (h *Handler) GetLegalHold(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	hld, err := h.store.FindLegalHoldByID(r.Context(), chi.URLParam(r, "id"), tenantID)
	if err != nil {
		if errors.Is(err, domain.ErrLegalHoldNotFound) {
			writeError(w, http.StatusNotFound, "legal hold not found")
			return
		}
		if errors.Is(err, domain.ErrTenantMissing) {
			writeError(w, http.StatusUnauthorized, "X-Tenant-Id header is required")
			return
		}
		h.logger.Error("get legal hold failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to get legal hold")
		return
	}

	scope := ""
	if hld.TenantID != nil {
		scope = *hld.TenantID
	}
	if !h.authorize(w, r, principalID, scope, LegalHoldRead) {
		return
	}

	writeJSON(w, http.StatusOK, hld)
}

// ListRetentionPolicies handles GET /v1/retention-policies.
//
// This service had no list endpoint at all — only create, and resolve for one
// record class at a time. Resolve answers "may I delete this particular thing";
// it cannot answer "what retention rules is this tenant operating under", which
// is the question an operator or auditor actually opens a console to ask.
func (h *Handler) ListRetentionPolicies(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	status := q.Get("policy_status")
	if status != "" && status != "ACTIVE" && status != "SUPERSEDED" && status != "RETIRED" {
		// Refused rather than ignored. A misspelled filter that is silently
		// dropped returns the whole register and reads as "no filter applied";
		// one that is silently honoured as "match nothing" reads as "this tenant
		// has no policies". Both mislead, in opposite directions.
		writeError(w, http.StatusBadRequest, "policy_status must be ACTIVE, SUPERSEDED or RETIRED")
		return
	}

	limit, offset, ok := pageParams(w, r)
	if !ok {
		return
	}

	policies, err := h.store.ListRetentionPolicies(r.Context(), domain.RetentionPolicyFilter{
		CallerTenantID: tenantID,
		RecordClass:    q.Get("record_class"),
		PolicyStatus:   status,
		Limit:          limit,
		Offset:         offset,
	})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidFilter) {
			writeError(w, http.StatusBadRequest, "limit must be 1-500 and offset must not be negative")
			return
		}
		h.logger.Error("list retention policies failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to list retention policies")
		return
	}
	writeJSON(w, http.StatusOK, policies)
}

// ListLegalHolds handles GET /v1/legal-holds.
//
// The register an operator needs before believing any deletion is safe: an
// active hold overrides every retention policy, and without this endpoint the
// only way to discover one was to already know its id.
func (h *Handler) ListLegalHolds(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	status := q.Get("hold_status")
	if status != "" && status != "ACTIVE" && status != "RELEASED" {
		writeError(w, http.StatusBadRequest, "hold_status must be ACTIVE or RELEASED")
		return
	}

	limit, offset, ok := pageParams(w, r)
	if !ok {
		return
	}

	holds, err := h.store.ListLegalHolds(r.Context(), domain.LegalHoldFilter{
		CallerTenantID: tenantID,
		HoldStatus:     status,
		RecordClass:    q.Get("record_class"),
		Limit:          limit,
		Offset:         offset,
	})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidFilter) {
			writeError(w, http.StatusBadRequest, "limit must be 1-500 and offset must not be negative")
			return
		}
		h.logger.Error("list legal holds failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to list legal holds")
		return
	}
	writeJSON(w, http.StatusOK, holds)
}

// requireTenant reads the gateway-verified tenant scope.
//
// An absent header is 401, never a default. Defaulting it is how a dropped
// header becomes an unscoped read of every tenant's legal holds — the exact
// shape of document-vault-svc's tenant filter that "switched itself off when the
// header was absent".
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := r.Header.Get("X-Tenant-Id")
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "X-Tenant-Id header is required")
		return "", false
	}
	return tenantID, true
}

// pageParams parses AND range-checks limit/offset.
//
// The range check belongs here and not only in the Postgres store. It was
// briefly in the store alone, and a stub Store implementation then accepted
// ?limit=5000 and answered 200 — validating a REQUEST parameter inside one
// persistence implementation means every other implementation, and every test
// double, silently disagrees about what the API accepts. The store keeps its own
// bounds check as defence in depth for callers that reach it directly.
//
// A non-numeric value is refused rather than treated as absent, and an
// out-of-range one is refused rather than clamped: a caller who asked for 5000
// rows and silently received 500 would read a truncated register as a complete
// one.
func pageParams(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	// limit 0 means "not supplied, use the store default". An explicitly
	// supplied limit=0 is a request for no rows, which is a mistake rather than
	// an intent, so presence is tracked separately from value — otherwise the
	// absent sentinel and a real 0 are indistinguishable.
	limit, offset := 0, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "limit must be an integer")
			return 0, 0, false
		}
		if v < 1 || v > 500 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return 0, 0, false
		}
		limit = v
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "offset must be an integer")
			return 0, 0, false
		}
		if v < 0 {
			writeError(w, http.StatusBadRequest, "offset must not be negative")
			return 0, 0, false
		}
		offset = v
	}
	return limit, offset, true
}

// ReleaseLegalHold handles POST /v1/legal-holds/{id}/release — doc7 §J3's
// "release approval". Legal only from ACTIVE.
func (h *Handler) ReleaseLegalHold(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.ReleaseLegalHoldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ReleaseApprovedByPrincipalID == "" {
		writeError(w, http.StatusBadRequest, "release_approved_by_principal_id is required")
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	// Scoped by the caller's verified tenant, so a hold in another tenant is a
	// 404 here and the authorize call below never sees it. The authorization
	// check against the hold's own tenant remains the real gate; this stops the
	// lookup that feeds it from reading across tenants in the first place.
	existing, err := h.store.FindLegalHoldByID(r.Context(), id, tenantID)
	if err != nil {
		if errors.Is(err, domain.ErrLegalHoldNotFound) {
			writeError(w, http.StatusNotFound, "legal hold not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch legal hold")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	scope := ""
	if existing.TenantID != nil {
		scope = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, scope, LegalHoldRelease) {
		return
	}

	released, err := h.store.ReleaseLegalHold(r.Context(), id, tenantID, principalID, req.ReleaseApprovedByPrincipalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrLegalHoldNotFound):
			writeError(w, http.StatusNotFound, "legal hold not found")
		case errors.Is(err, domain.ErrHoldNotActive):
			writeError(w, http.StatusConflict, "legal hold is not currently active")
		default:
			h.logger.Error("release legal hold failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "failed to release legal hold")
		}
		return
	}

	tenantForEvent := ""
	if released.TenantID != nil {
		tenantForEvent = *released.TenantID
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "legal_hold.released", EntityID: released.LegalHoldID, TenantID: tenantForEvent,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: released,
	})
	writeJSON(w, http.StatusOK, released)
}

// Resolve handles GET /v1/retention/resolve?record_class=&jurisdiction_code=&tenant_id=&entity_ref=
// — the check every caller makes before deleting/exporting/migrating a
// record. No authz gate: a read-only check every service needs cheaply
// and often, same posture as kill-switch-registry-svc's resolve endpoint.
func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	recordClass := q.Get("record_class")
	if recordClass == "" {
		writeError(w, http.StatusBadRequest, "record_class is required")
		return
	}
	jurisdictionCode := strPtrOrNil(q.Get("jurisdiction_code"))
	entityRef := strPtrOrNil(q.Get("entity_ref"))

	tenantID, ok := h.resolveTenantScope(w, r, q.Get("tenant_id"))
	if !ok {
		return
	}

	result, err := h.store.Resolve(r.Context(), recordClass, jurisdictionCode, tenantID, entityRef)
	if err != nil {
		h.logger.Error("resolve retention failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to resolve retention")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
