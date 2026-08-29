// Package handler exposes supplier-financial-profile-svc's REST API — AP-01.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/supplier-financial-profile-svc/internal/authz"
	"zoiko.io/supplier-financial-profile-svc/internal/domain"
	"zoiko.io/supplier-financial-profile-svc/internal/events"
	svcmiddleware "zoiko.io/supplier-financial-profile-svc/internal/middleware"
	"zoiko.io/supplier-financial-profile-svc/internal/store"
)

// Action constants — AP-01's own contract's "Authorization / permissions"
// line, adapted to this platform's SCREAMING_SNAKE_CASE convention (see
// master-register-findings-2026-08-27.md §2.5: reviewed and declined to
// rename to the new spec docs' dotted-lowercase convention platform-wide).
const (
	SupplierFinancialRead        = "SUPPLIER_FINANCIAL_READ"
	SupplierFinancialManage      = "SUPPLIER_FINANCIAL_MANAGE"
	SupplierHoldManage           = "SUPPLIER_HOLD_MANAGE"
	SupplierTermsManage          = "SUPPLIER_TERMS_MANAGE"
	SupplierProfileApproveChange = "SUPPLIER_PROFILE_APPROVE_CHANGE"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

// AuthzChecker is the real dependency on authorization-svc, including its
// dynamic own-object SoD layer — see internal/authz's package doc comment.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
}

type Handler struct {
	store store.Store
	pub   events.Publisher
	authz AuthzChecker
	log   *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/ap01/supplier-financial-profiles", func(r chi.Router) {
		r.Post("/", h.CreateProfile)
		r.Get("/", h.ListProfiles)
		r.Get("/{profileID}", h.GetProfile)
		r.Post("/{profileID}/activate", h.ActivateProfile)
		r.Post("/{profileID}/amend", h.AmendProfile)
		r.Post("/{profileID}/hold", h.PlaceHold)
		r.Post("/{profileID}/release-hold", h.ReleaseHold)
		r.Post("/{profileID}/retire", h.RetireProfile)

		r.Post("/{profileID}/payment-terms", h.ChangePaymentTerms)
		r.Get("/{profileID}/payment-terms", h.ListPaymentTerms)

		r.Post("/{profileID}/high-risk-changes", h.ProposeHighRiskChange)
		r.Get("/{profileID}/change-events", h.ListChangeEvents)
	})
	r.Route("/ap01/high-risk-changes", func(r chi.Router) {
		r.Post("/{changeRequestID}/decide", h.DecideHighRiskChange)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		return h.handleAuthzErr(w, err)
	}
	return true
}

func (h *Handler) handleAuthzErr(w http.ResponseWriter, err error) bool {
	if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "not authorized to perform this action")
		return false
	}
	h.log.Error("authorization check failed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
	return false
}

// ── profiles ─────────────────────────────────────────────────────────────────

func (h *Handler) CreateProfile(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.SupplierRef == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id and supplier_ref are required")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if req.TenantID != "" && req.TenantID != verifiedTenant {
		writeError(w, http.StatusForbidden, "tenant_id does not match the verified X-Tenant-Id")
		return
	}
	tenantID := req.TenantID
	if tenantID == "" {
		tenantID = verifiedTenant
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, SupplierFinancialManage) {
		return
	}

	p, err := h.store.CreateProfile(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.log.Error("CreateProfile: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "supplier_financial_profile.created", EntityID: p.ProfileID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: p,
	})
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) GetProfile(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	p, err := h.store.FindProfile(r.Context(), profileID)
	if err != nil {
		if errors.Is(err, domain.ErrProfileNotFound) {
			writeError(w, http.StatusNotFound, "supplier financial profile not found")
			return
		}
		h.log.Error("GetProfile: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) ListProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := h.store.ListProfiles(r.Context())
	if err != nil {
		h.log.Error("ListProfiles: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if profiles == nil {
		profiles = []domain.SupplierFinancialProfile{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": profiles, "count": len(profiles)})
}

func (h *Handler) fetchProfileForAuth(w http.ResponseWriter, r *http.Request, profileID string) (*domain.SupplierFinancialProfile, bool) {
	p, err := h.store.FindProfile(r.Context(), profileID)
	if err != nil {
		if errors.Is(err, domain.ErrProfileNotFound) {
			writeError(w, http.StatusNotFound, "supplier financial profile not found")
			return nil, false
		}
		h.log.Error("fetchProfileForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return p, true
}

func (h *Handler) ActivateProfile(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchProfileForAuth(w, r, profileID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, SupplierFinancialManage) {
		return
	}

	p, err := h.store.ActivateProfile(r.Context(), profileID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "profile is not in the required DRAFT state")
			return
		}
		h.log.Error("ActivateProfile: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) AmendProfile(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	var req domain.AmendProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchProfileForAuth(w, r, profileID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, SupplierFinancialManage) {
		return
	}

	p, err := h.store.AmendProfile(r.Context(), profileID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrProfileNotFound) {
			writeError(w, http.StatusNotFound, "supplier financial profile not found")
			return
		}
		h.log.Error("AmendProfile: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) PlaceHold(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	var req domain.PlaceHoldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchProfileForAuth(w, r, profileID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, SupplierHoldManage) {
		return
	}

	p, err := h.store.PlaceHold(r.Context(), profileID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "profile is not in the required ACTIVE state")
			return
		}
		h.log.Error("PlaceHold: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "supplier_hold.placed", EntityID: p.ProfileID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: p,
	})
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) ReleaseHold(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchProfileForAuth(w, r, profileID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, SupplierHoldManage) {
		return
	}

	p, err := h.store.ReleaseHold(r.Context(), profileID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "profile is not in the required ON_HOLD state")
			return
		}
		h.log.Error("ReleaseHold: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "supplier_hold.released", EntityID: p.ProfileID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: p,
	})
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) RetireProfile(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	var req domain.RetireProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchProfileForAuth(w, r, profileID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, SupplierFinancialManage) {
		return
	}

	p, err := h.store.RetireProfile(r.Context(), profileID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "profile is already RETIRED")
			return
		}
		h.log.Error("RetireProfile: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── payment terms ────────────────────────────────────────────────────────────

// ChangePaymentTerms handles POST .../payment-terms. Overlapping
// effective periods are rejected by a genuine database EXCLUDE
// constraint (negative-path scenario #2 from AP-01's own acceptance
// table) — see the migration's own comment.
func (h *Handler) ChangePaymentTerms(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	var req domain.ChangePaymentTermsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TermsCode == "" || req.EffectiveFrom.IsZero() {
		writeError(w, http.StatusBadRequest, "terms_code and effective_from are required")
		return
	}
	if req.EffectiveTo != nil && !req.EffectiveTo.After(req.EffectiveFrom) {
		writeError(w, http.StatusBadRequest, "effective_to must be after effective_from")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchProfileForAuth(w, r, profileID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, SupplierTermsManage) {
		return
	}

	t, err := h.store.ChangePaymentTerms(r.Context(), profileID, req, principalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrProfileNotFound):
			writeError(w, http.StatusNotFound, "supplier financial profile not found")
		case errors.Is(err, domain.ErrOverlappingPaymentTerms):
			writeError(w, http.StatusConflict, "payment terms period overlaps an existing effective period")
		default:
			h.log.Error("ChangePaymentTerms: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "supplier_payment_terms.changed", EntityID: profileID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: t,
	})
	writeJSON(w, http.StatusCreated, t)
}

func (h *Handler) ListPaymentTerms(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	terms, err := h.store.ListPaymentTerms(r.Context(), profileID)
	if err != nil {
		h.log.Error("ListPaymentTerms: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if terms == nil {
		terms = []domain.PaymentTermsPeriod{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": terms, "count": len(terms)})
}

// ── high-risk changes (own-object SoD) ──────────────────────────────────────

// ProposeHighRiskChange handles POST .../high-risk-changes. This does not
// apply the change — it only records the proposal. Applying it requires
// a SEPARATE call to DecideHighRiskChange by a different principal — see
// that handler's doc comment for the actual SoD enforcement.
func (h *Handler) ProposeHighRiskChange(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	var req domain.ProposeHighRiskChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !req.Field.Valid() {
		writeError(w, http.StatusBadRequest, "field must be PAYEE_REFERENCE or PAYMENT_METHOD_PREFERENCE")
		return
	}
	if req.NewValue == "" {
		writeError(w, http.StatusBadRequest, "new_value is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchProfileForAuth(w, r, profileID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, SupplierFinancialManage) {
		return
	}

	c, err := h.store.ProposeHighRiskChange(r.Context(), profileID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrProfileNotFound) {
			writeError(w, http.StatusNotFound, "supplier financial profile not found")
			return
		}
		h.log.Error("ProposeHighRiskChange: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// DecideHighRiskChange handles POST /ap01/high-risk-changes/{id}/decide —
// AP-01's own SoD line, enforced for real: "users able to change payee
// identity cannot authorize resulting payment without independent
// control." This is the proposal/approval half of that line (the
// payment-authorization half needs AP-10, which doesn't exist). The
// deciding principal is checked against authorization-svc's dynamic
// own-object SoD layer with the PROPOSER as resource_owner_principal_id
// — if they're the same principal, authorization-svc itself denies it,
// not application logic duplicated here.
func (h *Handler) DecideHighRiskChange(w http.ResponseWriter, r *http.Request) {
	changeRequestID := chi.URLParam(r, "changeRequestID")
	var req domain.DecideHighRiskChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	cr, err := h.store.FindChangeRequest(r.Context(), changeRequestID)
	if err != nil {
		if errors.Is(err, domain.ErrChangeRequestNotFound) {
			writeError(w, http.StatusNotFound, "high-risk change request not found")
			return
		}
		h.log.Error("DecideHighRiskChange: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	profile, ok := h.fetchProfileForAuth(w, r, cr.ProfileID)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowedOwnObject(r.Context(), principalID, profile.LegalEntityID, SupplierProfileApproveChange, cr.ProposedByPrincipalID); err != nil {
		h.handleAuthzErr(w, err)
		return
	}

	decided, updatedProfile, err := h.store.DecideHighRiskChange(r.Context(), changeRequestID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrChangeRequestNotPending) {
			writeError(w, http.StatusConflict, "high-risk change request is not pending")
			return
		}
		h.log.Error("DecideHighRiskChange: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "supplier_high_risk_change.decided", EntityID: decided.ProfileID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: decided,
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"change_request": decided, "profile": updatedProfile})
}

// ── change events ────────────────────────────────────────────────────────────

func (h *Handler) ListChangeEvents(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	events, err := h.store.ListChangeEvents(r.Context(), profileID)
	if err != nil {
		h.log.Error("ListChangeEvents: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if events == nil {
		events = []domain.ProfileChangeEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": events, "count": len(events)})
}
