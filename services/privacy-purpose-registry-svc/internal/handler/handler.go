// Package handler exposes privacy-purpose-registry-svc's REST API — PRV-01.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-purpose-registry-svc/internal/authz"
	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	"zoiko.io/privacy-purpose-registry-svc/internal/events"
	svcmiddleware "zoiko.io/privacy-purpose-registry-svc/internal/middleware"
	"zoiko.io/privacy-purpose-registry-svc/internal/store"
)

// Action constants passed to authorization-svc as action_type — SCREAMING_
// SNAKE_CASE, same convention as every other service in this platform
// (see master-register-findings-2026-08-27.md §2.5: the spec's own
// dotted-lowercase convention was reviewed and declined estate-wide, so
// this new service follows the codebase's actual convention, not the
// unadopted one).
const (
	PrivacyPurposeCreate  = "PRIVACY_PURPOSE_CREATE"
	PrivacyPurposePublish = "PRIVACY_PURPOSE_PUBLISH"

	PrivacyActivityCreate   = "PRIVACY_ACTIVITY_CREATE"
	PrivacyActivityValidate = "PRIVACY_ACTIVITY_VALIDATE"
	PrivacyActivitySubmit   = "PRIVACY_ACTIVITY_SUBMIT"
	PrivacyActivityApprove  = "PRIVACY_ACTIVITY_APPROVE"
	PrivacyActivityActivate = "PRIVACY_ACTIVITY_ACTIVATE"
	PrivacyActivitySuspend  = "PRIVACY_ACTIVITY_SUSPEND"
	PrivacyActivityResume   = "PRIVACY_ACTIVITY_RESUME"
	PrivacyActivityRetire   = "PRIVACY_ACTIVITY_RETIRE"
)

// platformScopeID mirrors authorization-svc's own constant of the same
// name (see retention-registry-svc, authorization-svc) — used as the
// authz scope for a platform-wide (Zoiko-as-controller) purpose or
// activity, which has no owning tenant to scope against.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthzChecker
	log       *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, log *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/privacy/purposes", func(r chi.Router) {
		r.Post("/", h.CreatePurpose)
		r.Get("/", h.ListPurposes)
		r.Get("/{purposeID}", h.GetPurpose)
		r.Post("/{purposeID}/versions", h.CreatePurposeVersion)
		r.Post("/{purposeID}/versions/{versionID}/publish", h.PublishPurposeVersion)
	})

	r.Route("/privacy/processing-activities", func(r chi.Router) {
		r.Post("/", h.CreateActivity)
		r.Get("/{activityID}", h.GetActivity)
		r.Post("/{activityID}/versions", h.CreateActivityVersion)
		r.Get("/{activityID}/versions/{versionID}", h.GetActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/validate", h.ValidateActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/submit", h.SubmitActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/approve", h.ApproveActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/reject", h.RejectActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/activate", h.ActivateActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/suspend", h.SuspendActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/resume", h.ResumeActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/retire", h.RetireActivityVersion)
	})

	r.Get("/privacy/ropa", h.ListROPA)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// requirePrincipal reads the caller's identity from X-Principal-Id, set by
// the gateway after identity verification. A request with no resolved
// principal never passed identity verification — fail closed with 401.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// authorize asks authorization-svc whether principalID may perform
// actionType within tenantID's scope (or the platform scope, if
// tenantID is empty — PRV-I01/I02/I03/I04: possession, tenant
// membership, and commercial entitlement are all independent of privacy
// permission, so this check is its own gate, never inferred from the
// caller merely being able to reach the tenant).
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, tenantID, actionType string) bool {
	scope := platformScopeID
	if tenantID != "" {
		scope = tenantID
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, scope, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}

func parseAsOf(r *http.Request) time.Time {
	raw := r.URL.Query().Get("as_of")
	if raw == "" {
		return time.Now().UTC()
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Now().UTC()
	}
	return t
}

// ── purposes ─────────────────────────────────────────────────────────────────

// CreatePurpose handles POST /privacy/purposes.
func (h *Handler) CreatePurpose(w http.ResponseWriter, r *http.Request) {
	var req domain.CreatePurposeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Statement == "" || req.CompatibilityClass == "" {
		writeError(w, http.StatusBadRequest, "statement and compatibility_class are required")
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
	if !h.authorize(w, r, principalID, tenantID, PrivacyPurposeCreate) {
		return
	}

	_, version, err := h.store.CreatePurpose(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.log.Error("CreatePurpose: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, version)
}

// CreatePurposeVersion handles POST /privacy/purposes/{purposeID}/versions
// — "create successor draft from explicit parent version" (§9.1).
func (h *Handler) CreatePurposeVersion(w http.ResponseWriter, r *http.Request) {
	purposeID := chi.URLParam(r, "purposeID")

	var req domain.CreatePurposeVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ParentVersionID == "" || req.Statement == "" || req.CompatibilityClass == "" {
		writeError(w, http.StatusBadRequest, "parent_version_id, statement and compatibility_class are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// Authorize against the same action as create — a new version is the
	// same privilege as bringing the purpose into existence in the first
	// place, not a lesser one.
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyPurposeCreate) {
		return
	}

	version, err := h.store.CreatePurposeVersion(r.Context(), purposeID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrPurposeVersionNotFound) {
			writeError(w, http.StatusNotFound, "parent purpose version not found")
			return
		}
		h.log.Error("CreatePurposeVersion: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, version)
}

// PublishPurposeVersion handles
// POST /privacy/purposes/{purposeID}/versions/{versionID}/publish.
// PRV-I06: once published, a purpose version is immutable — enforced
// again at the database layer (migration 000002's trigger), not just here.
func (h *Handler) PublishPurposeVersion(w http.ResponseWriter, r *http.Request) {
	purposeID := chi.URLParam(r, "purposeID")
	versionID := chi.URLParam(r, "versionID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyPurposePublish) {
		return
	}

	version, err := h.store.PublishPurposeVersion(r.Context(), purposeID, versionID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPurposeVersionNotFound):
			writeError(w, http.StatusNotFound, "purpose version not found")
		case errors.Is(err, domain.ErrPurposeAlreadyPublished):
			writeError(w, http.StatusConflict, "purpose version is already published")
		default:
			h.log.Error("PublishPurposeVersion: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.purpose.published", EntityID: version.PurposeID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: version,
	})
	writeJSON(w, http.StatusOK, version)
}

// GetPurpose handles GET /privacy/purposes/{purposeID}?as_of= — resolves
// the exact valid/known published version for historical evidence.
// Unauthenticated beyond tenant scoping: reads are scoped, not
// authorized, same posture accounts-receivable-svc documents for its own
// read routes — there is no PRIVACY_PURPOSE_READ action because a
// published purpose is, by definition, meant to be visible platform- or
// tenant-wide (it is what a notice/consent flow shows a data subject).
func (h *Handler) GetPurpose(w http.ResponseWriter, r *http.Request) {
	purposeID := chi.URLParam(r, "purposeID")
	version, err := h.store.ResolvePurposeAsOf(r.Context(), purposeID, parseAsOf(r))
	if err != nil {
		if errors.Is(err, domain.ErrPurposeNotFound) {
			writeError(w, http.StatusNotFound, "purpose not found")
			return
		}
		h.log.Error("GetPurpose: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, version)
}

// ListPurposes handles GET /privacy/purposes — every currently PUBLISHED
// purpose visible in this tenant scope (platform-wide included).
func (h *Handler) ListPurposes(w http.ResponseWriter, r *http.Request) {
	versions, err := h.store.ListPurposes(r.Context())
	if err != nil {
		h.log.Error("ListPurposes: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if versions == nil {
		versions = []domain.PurposeVersion{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": versions, "count": len(versions)})
}

// ── processing activities ────────────────────────────────────────────────────

func (h *Handler) CreateActivity(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateActivityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !domain.PrivacyRole(req.PrivacyRole).Valid() {
		writeError(w, http.StatusBadRequest, "privacy_role must be one of CONTROLLER, PROCESSOR, JOINT_CONTROLLER")
		return
	}
	if req.Owner == "" {
		writeError(w, http.StatusBadRequest, "owner is required")
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
	if !h.authorize(w, r, principalID, tenantID, PrivacyActivityCreate) {
		return
	}

	_, version, err := h.store.CreateActivity(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.log.Error("CreateActivity: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, version)
}

// CreateActivityVersion handles
// POST /privacy/processing-activities/{activityID}/versions — the spec's
// "create successor draft from explicit parent version." This is how the
// Figure 4 "reject/fix loop" is actually taken: a REJECTED version is
// never resurrected (PRV-I20), a caller creates a new DRAFT version from
// it instead.
func (h *Handler) CreateActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")

	var req domain.CreateActivityVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ParentVersionID == "" {
		writeError(w, http.StatusBadRequest, "parent_version_id is required")
		return
	}
	if !domain.PrivacyRole(req.PrivacyRole).Valid() {
		writeError(w, http.StatusBadRequest, "privacy_role must be one of CONTROLLER, PROCESSOR, JOINT_CONTROLLER")
		return
	}
	if req.Owner == "" {
		writeError(w, http.StatusBadRequest, "owner is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyActivityCreate) {
		return
	}

	version, err := h.store.CreateActivityVersion(r.Context(), activityID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrActivityVersionNotFound) {
			writeError(w, http.StatusNotFound, "parent processing activity version not found")
			return
		}
		h.log.Error("CreateActivityVersion: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, version)
}

func (h *Handler) GetActivity(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	version, err := h.store.ResolveActivityAsOf(r.Context(), activityID, parseAsOf(r))
	if err != nil {
		if errors.Is(err, domain.ErrActivityNotFound) {
			writeError(w, http.StatusNotFound, "processing activity not found")
			return
		}
		h.log.Error("GetActivity: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, version)
}

func (h *Handler) GetActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")
	version, err := h.store.FindActivityVersion(r.Context(), activityID, versionID)
	if err != nil {
		if errors.Is(err, domain.ErrActivityVersionNotFound) {
			writeError(w, http.StatusNotFound, "processing activity version not found")
			return
		}
		h.log.Error("GetActivityVersion: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, version)
}

// ValidateActivityVersion handles
// POST .../{activityID}/versions/{versionID}/validate — "deterministic
// validation/dependency findings, no activation" (§9.1). Structural only:
// see domain package's doc comment for exactly what this does and does
// not check, and why.
func (h *Handler) ValidateActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyActivityValidate) {
		return
	}

	existing, err := h.store.FindActivityVersion(r.Context(), activityID, versionID)
	if err != nil {
		if errors.Is(err, domain.ErrActivityVersionNotFound) {
			writeError(w, http.StatusNotFound, "processing activity version not found")
			return
		}
		h.log.Error("ValidateActivityVersion: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if existing.VersionStatus != domain.ActivityStatusDraft {
		writeError(w, http.StatusConflict, "only a DRAFT version may be validated")
		return
	}

	findings := h.runStructuralValidation(r.Context(), existing)

	updated, err := h.store.SetValidationOutcome(r.Context(), activityID, versionID, findings)
	if err != nil {
		if errors.Is(err, domain.ErrActivityVersionNotFound) {
			writeError(w, http.StatusNotFound, "processing activity version not found")
			return
		}
		h.log.Error("ValidateActivityVersion: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// runStructuralValidation implements the §32 error-code contract's
// structural subset. PRV-I13: any finding at all means the version stays
// DRAFT — findings are never partial-permit.
func (h *Handler) runStructuralValidation(ctx context.Context, v *domain.ProcessingActivityVersion) []domain.ValidationFinding {
	var findings []domain.ValidationFinding

	if len(v.PurposeIDs) == 0 {
		findings = append(findings, domain.ValidationFinding{
			Code: "PRV-001", Field: "purpose_ids", Message: "at least one purpose_id is required",
		})
	}
	for _, pid := range v.PurposeIDs {
		published, err := h.store.IsPurposePublished(ctx, pid)
		if err != nil || !published {
			findings = append(findings, domain.ValidationFinding{
				Code: "PRV-001", Field: "purpose_ids",
				Message: "purpose " + pid + " is not a registered, published purpose",
			})
		}
	}
	if len(v.Jurisdictions) == 0 {
		findings = append(findings, domain.ValidationFinding{
			Code: "PRV-004", Field: "jurisdictions", Message: "at least one jurisdiction is required",
		})
	}
	if len(v.SubjectClasses) == 0 {
		findings = append(findings, domain.ValidationFinding{
			Code: "PRV-019", Field: "subject_classes", Message: "at least one subject class is required",
		})
	}
	if len(v.DataCategories) == 0 {
		findings = append(findings, domain.ValidationFinding{
			Code: "PRV-019", Field: "data_categories", Message: "at least one data category is required",
		})
	}
	return findings
}

// transitionAction is the common shape for the simple one-step lifecycle
// actions (submit/approve/activate is handled separately because it also
// sets effective_from; reject is handled separately because it requires a
// reason).
func (h *Handler) transitionAction(w http.ResponseWriter, r *http.Request, actionType string, from, to domain.ActivityVersionStatus, eventType string) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, actionType) {
		return
	}

	if !domain.ValidActivityTransition(from, to) {
		// Defensive — this only happens if a route is wired to a
		// transition that domain.activityTransitions doesn't declare.
		writeError(w, http.StatusInternalServerError, "misconfigured transition")
		return
	}

	updated, err := h.store.TransitionActivity(r.Context(), activityID, versionID, from, to)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrActivityVersionNotFound):
			writeError(w, http.StatusNotFound, "processing activity version not found")
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "version is not in the required "+string(from)+" state")
		default:
			h.log.Error("transitionAction: store unavailable", zap.String("action", actionType), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: eventType, EntityID: updated.ActivityID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) SubmitActivityVersion(w http.ResponseWriter, r *http.Request) {
	h.transitionAction(w, r, PrivacyActivitySubmit, domain.ActivityStatusValidated, domain.ActivityStatusSubmitted, "privacy.processing_activity.submitted")
}

func (h *Handler) ApproveActivityVersion(w http.ResponseWriter, r *http.Request) {
	h.transitionAction(w, r, PrivacyActivityApprove, domain.ActivityStatusSubmitted, domain.ActivityStatusApproved, "privacy.processing_activity.approved")
}

func (h *Handler) SuspendActivityVersion(w http.ResponseWriter, r *http.Request) {
	h.transitionAction(w, r, PrivacyActivitySuspend, domain.ActivityStatusActive, domain.ActivityStatusSuspended, "privacy.processing_activity.suspended")
}

func (h *Handler) ResumeActivityVersion(w http.ResponseWriter, r *http.Request) {
	h.transitionAction(w, r, PrivacyActivityResume, domain.ActivityStatusSuspended, domain.ActivityStatusActive, "privacy.processing_activity.resumed")
}

// RetireActivityVersion handles .../retire. Retire is legal from either
// ACTIVE or SUSPENDED (Figure 4's branches both terminate here), so it
// cannot use the single-`from` transitionAction helper.
func (h *Handler) RetireActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyActivityRetire) {
		return
	}

	existing, err := h.store.FindActivityVersion(r.Context(), activityID, versionID)
	if err != nil {
		if errors.Is(err, domain.ErrActivityVersionNotFound) {
			writeError(w, http.StatusNotFound, "processing activity version not found")
			return
		}
		h.log.Error("RetireActivityVersion: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if !domain.ValidActivityTransition(existing.VersionStatus, domain.ActivityStatusRetired) {
		writeError(w, http.StatusConflict, "version must be ACTIVE or SUSPENDED to retire")
		return
	}

	updated, err := h.store.TransitionActivity(r.Context(), activityID, versionID, existing.VersionStatus, domain.ActivityStatusRetired)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrActivityVersionNotFound):
			writeError(w, http.StatusNotFound, "processing activity version not found")
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "version must be ACTIVE or SUSPENDED to retire")
		default:
			h.log.Error("RetireActivityVersion: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.processing_activity.retired", EntityID: updated.ActivityID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

// RejectActivityVersion handles .../reject — requires a reason, which
// TransitionActivity's generic shape doesn't carry.
func (h *Handler) RejectActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")

	var req domain.RejectActivityRequest
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
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	// Reject uses the same authorization action as Approve — both are the
	// outcome of the same review privilege, "may render a verdict on a
	// submitted activity."
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyActivityApprove) {
		return
	}

	updated, err := h.store.RejectActivity(r.Context(), activityID, versionID, req.Reason)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrActivityVersionNotFound):
			writeError(w, http.StatusNotFound, "processing activity version not found")
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "version is not in the required SUBMITTED state")
		default:
			h.log.Error("RejectActivityVersion: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.processing_activity.rejected", EntityID: updated.ActivityID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

// ActivateActivityVersion handles .../activate — requires setting
// effective_from, which TransitionActivity's generic shape doesn't carry.
func (h *Handler) ActivateActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")

	var req domain.ActivateActivityRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	effectiveFrom := time.Now().UTC()
	if req.EffectiveFrom != nil {
		effectiveFrom = *req.EffectiveFrom
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyActivityActivate) {
		return
	}

	updated, err := h.store.ActivateActivity(r.Context(), activityID, versionID, effectiveFrom)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrActivityVersionNotFound):
			writeError(w, http.StatusNotFound, "processing activity version not found")
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "version is not in the required APPROVED state")
		default:
			h.log.Error("ActivateActivityVersion: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	// privacy.processing_activity.activated is the exact event name
	// ZS-EVENT-001/§19 specifies for this transition.
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.processing_activity.activated", EntityID: updated.ActivityID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

// ListROPA handles GET /privacy/ropa — the role/jurisdiction-filtered
// processing inventory projection (§9.1). Reads are tenant-scoped only,
// same posture as GetPurpose/ListPurposes.
func (h *Handler) ListROPA(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("role")
	jurisdiction := r.URL.Query().Get("jurisdiction")

	versions, err := h.store.ListActiveActivities(r.Context(), role, jurisdiction)
	if err != nil {
		h.log.Error("ListROPA: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if versions == nil {
		versions = []domain.ProcessingActivityVersion{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": versions, "count": len(versions)})
}
