// Package handler exposes privacy-purpose-registry-svc's REST API — PRV-01.
package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	PrivacyActivityReject   = "PRIVACY_ACTIVITY_REJECT"
	PrivacyActivityActivate = "PRIVACY_ACTIVITY_ACTIVATE"
	PrivacyActivitySuspend  = "PRIVACY_ACTIVITY_SUSPEND"
	PrivacyActivityResume   = "PRIVACY_ACTIVITY_RESUME"
	PrivacyActivityRetire   = "PRIVACY_ACTIVITY_RETIRE"
)

// platformScopeID mirrors authorization-svc's platform entity UUID
// ("00000000-0000-0000-0000-00000000f001"). Purpose records may be platform-wide or
// tenant-scoped; when an incoming request carries an empty tenant (or
// explicitly claims platform scope), this is the scope passed to the
// authorization check.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Handler wires HTTP routes to domain logic and data stores.
type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthzChecker
	log       *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, log *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, log: log}
}

// RegisterRoutes registers all routes for PRV-01 on the provided router.
func RegisterRoutes(r chi.Router, h *Handler) {
	h.Routes(r)
}

// Routes registers the REST surface on the provided Chi router.
func (h *Handler) Routes(r chi.Router) {
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

		// Version-explicit routes
		r.Post("/{activityID}/versions/{versionID}/validate", h.ValidateActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/submit", h.SubmitActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/approve", h.ApproveActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/reject", h.RejectActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/activate", h.ActivateActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/suspend", h.SuspendActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/resume", h.ResumeActivityVersion)
		r.Post("/{activityID}/versions/{versionID}/retire", h.RetireActivityVersion)

		// Canonical latest-version route aliases (§9.1)
		r.Post("/{activityID}/validate", h.ValidateLatestActivity)
		r.Post("/{activityID}/submit", h.SubmitLatestActivity)
		r.Post("/{activityID}/approve", h.ApproveLatestActivity)
		r.Post("/{activityID}/reject", h.RejectLatestActivity)
		r.Post("/{activityID}/activate", h.ActivateLatestActivity)
		r.Post("/{activityID}/suspend", h.SuspendLatestActivity)
		r.Post("/{activityID}/resume", h.ResumeLatestActivity)
		r.Post("/{activityID}/retire", h.RetireLatestActivity)
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

func readBodyBytes(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return bodyBytes, nil
}

func computeRequestHash(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte(path))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// checkIdempotency checks if the request has an Idempotency-Key header (§18.1).
func (h *Handler) checkIdempotency(w http.ResponseWriter, r *http.Request, tenantID, principalID string, body []byte) (replayed bool, handled bool, key string, hash string) {
	key = r.Header.Get("Idempotency-Key")
	if key == "" {
		return false, false, "", ""
	}
	hash = computeRequestHash(r.Method, r.URL.Path, body)
	rec, err := h.store.GetIdempotency(r.Context(), tenantID, principalID, key)
	if err != nil {
		h.log.Error("GetIdempotency failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return false, true, key, hash
	}
	if rec != nil {
		if rec.RequestHash != hash {
			writeError(w, http.StatusConflict, "idempotency key reused with different request payload")
			return false, true, key, hash
		}
		w.Header().Set("Idempotency-Replay", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.ResponseStatus)
		_, _ = w.Write(rec.ResponseBody)
		return true, true, key, hash
	}
	return false, false, key, hash
}

// writeJSONWithIdempotency serializes the payload, stores the idempotency record (if key provided),
// and writes the HTTP response.
func (h *Handler) writeJSONWithIdempotency(ctx context.Context, w http.ResponseWriter, tenantID, principalID, op, key, hash string, status int, v interface{}) {
	payload, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to serialize response")
		return
	}
	if key != "" && hash != "" {
		var tID *string
		if tenantID != "" {
			tID = &tenantID
		}
		if err := h.store.SaveIdempotency(ctx, domain.IdempotencyRecord{
			IdempotencyKey: key,
			TenantID:       tID,
			PrincipalID:    principalID,
			Operation:      op,
			RequestHash:    hash,
			ResponseStatus: status,
			ResponseBody:   payload,
			CreatedAt:      time.Now().UTC(),
		}); err != nil {
			h.log.Warn("SaveIdempotency failed", zap.String("key", key), zap.Error(err))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
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
// tenantID is empty).
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
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Now().UTC()
}

func (h *Handler) resolveLatestVersion(w http.ResponseWriter, r *http.Request, activityID string) (*domain.ProcessingActivityVersion, bool) {
	latest, err := h.store.FindLatestActivityVersion(r.Context(), activityID)
	if err != nil {
		if errors.Is(err, domain.ErrActivityNotFound) || errors.Is(err, domain.ErrActivityVersionNotFound) {
			writeError(w, http.StatusNotFound, "processing activity not found")
			return nil, false
		}
		h.log.Error("resolveLatestVersion: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return latest, true
}

// ── purposes ─────────────────────────────────────────────────────────────────

// CreatePurpose handles POST /privacy/purposes.
func (h *Handler) CreatePurpose(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.CreatePurposeRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
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

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
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
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "CreatePurpose", idemKey, reqHash, http.StatusCreated, version)
}

// CreatePurposeVersion handles POST /privacy/purposes/{purposeID}/versions.
func (h *Handler) CreatePurposeVersion(w http.ResponseWriter, r *http.Request) {
	purposeID := chi.URLParam(r, "purposeID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.CreatePurposeVersionRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ParentVersionID == "" || req.Statement == "" || req.CompatibilityClass == "" {
		writeError(w, http.StatusBadRequest, "parent_version_id, statement and compatibility_class are required")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

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
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "CreatePurposeVersion", idemKey, reqHash, http.StatusCreated, version)
}

// PublishPurposeVersion handles POST /privacy/purposes/{purposeID}/versions/{versionID}/publish.
// Segregation of Duties (Maker-Checker, §18): A maker cannot publish their own purpose version.
func (h *Handler) PublishPurposeVersion(w http.ResponseWriter, r *http.Request) {
	purposeID := chi.URLParam(r, "purposeID")
	versionID := chi.URLParam(r, "versionID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyPurposePublish) {
		return
	}

	// Segregation of Duties check
	existing, err := h.store.FindPurposeVersion(r.Context(), purposeID, versionID)
	if err != nil {
		if errors.Is(err, domain.ErrPurposeVersionNotFound) {
			writeError(w, http.StatusNotFound, "purpose version not found")
			return
		}
		h.log.Error("PublishPurposeVersion: FindPurposeVersion failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if existing.CreatedByPrincipalID != "" && existing.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "maker cannot publish their own purpose version (segregation of duties)")
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
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "PublishPurposeVersion", idemKey, reqHash, http.StatusOK, version)
}

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
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.CreateActivityRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
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
	if req.NoticeConsentDependency != "" && !domain.NoticeConsentDependency(req.NoticeConsentDependency).Valid() {
		writeError(w, http.StatusBadRequest, "notice_consent_dependency must be REQUIRED, NOT_REQUIRED, or CONDITIONAL")
		return
	}
	if req.DPIATIAStatus != "" && !domain.DPIATIAStatus(req.DPIATIAStatus).Valid() {
		writeError(w, http.StatusBadRequest, "dpia_tia_status must be RESOLVED, REVIEW_REQUIRED, or NOT_REQUIRED")
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

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
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
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "CreateActivity", idemKey, reqHash, http.StatusCreated, version)
}

func (h *Handler) CreateActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.CreateActivityVersionRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
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
	if req.NoticeConsentDependency != "" && !domain.NoticeConsentDependency(req.NoticeConsentDependency).Valid() {
		writeError(w, http.StatusBadRequest, "notice_consent_dependency must be REQUIRED, NOT_REQUIRED, or CONDITIONAL")
		return
	}
	if req.DPIATIAStatus != "" && !domain.DPIATIAStatus(req.DPIATIAStatus).Valid() {
		writeError(w, http.StatusBadRequest, "dpia_tia_status must be RESOLVED, REVIEW_REQUIRED, or NOT_REQUIRED")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

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
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "CreateActivityVersion", idemKey, reqHash, http.StatusCreated, version)
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

func (h *Handler) ValidateActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")
	h.validateActivityVersionInternal(w, r, activityID, versionID)
}

func (h *Handler) ValidateLatestActivity(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	latest, ok := h.resolveLatestVersion(w, r, activityID)
	if !ok {
		return
	}
	h.validateActivityVersionInternal(w, r, activityID, latest.ActivityVersionID)
}

func (h *Handler) validateActivityVersionInternal(w http.ResponseWriter, r *http.Request, activityID, versionID string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyActivityValidate) {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
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
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "ValidateActivity", idemKey, reqHash, http.StatusOK, updated)
}

// runStructuralValidation implements all 8 minimum activation gates (§8.2)
// using the exact error codes specified in §32:
// 1. Accountable owner (PRV-003)
// 2. Controller/processor role resolved (PRV-003)
// 3/4. Purpose statement & published purpose references (PRV-001)
// 5. Jurisdictions (PRV-004), subject classes and data categories (PRV-010)
// 6. Retention rule references identified (PRV-014)
// 7. Notice/consent dependency explicitly set (PRV-006)
// 8. DPIA/TIA requirement resolved or marked review-required (PRV-016)
// Any finding means the version stays DRAFT (PRV-I13: findings are never partial-permit).
func (h *Handler) runStructuralValidation(ctx context.Context, v *domain.ProcessingActivityVersion) []domain.ValidationFinding {
	var findings []domain.ValidationFinding

	// Gate 1: Accountable owner
	if v.Owner == "" {
		findings = append(findings, domain.ValidationFinding{
			Code: "PRV-003", Field: "owner", Message: "accountable owner is required",
		})
	}

	// Gate 2: Controller/processor role resolved
	if !domain.PrivacyRole(v.PrivacyRole).Valid() {
		findings = append(findings, domain.ValidationFinding{
			Code: "PRV-003", Field: "privacy_role", Message: "privacy_role must be one of CONTROLLER, PROCESSOR, JOINT_CONTROLLER",
		})
	}

	// Gate 3 & 4: Registered, published purpose references
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

	// Gate 5: Data subjects, categories, and jurisdictions enumerated (§32: PRV-004 for jurisdictions, PRV-010 for categories/subjects)
	if len(v.Jurisdictions) == 0 {
		findings = append(findings, domain.ValidationFinding{
			Code: "PRV-004", Field: "jurisdictions", Message: "at least one jurisdiction is required",
		})
	}
	if len(v.SubjectClasses) == 0 {
		findings = append(findings, domain.ValidationFinding{
			Code: "PRV-010", Field: "subject_classes", Message: "at least one subject class is required",
		})
	}
	if len(v.DataCategories) == 0 {
		findings = append(findings, domain.ValidationFinding{
			Code: "PRV-010", Field: "data_categories", Message: "at least one data category is required",
		})
	}

	// Gate 6: Retention/DRC rule references identified (§32: PRV-014)
	if len(v.RetentionRuleRefs) == 0 {
		findings = append(findings, domain.ValidationFinding{
			Code: "PRV-014", Field: "retention_rule_refs", Message: "at least one retention rule reference is required",
		})
	}

	// Gate 7: Notice/consent dependency explicitly set — never left implicit (§32: PRV-006)
	if v.NoticeConsentDependency == "" || !domain.NoticeConsentDependency(v.NoticeConsentDependency).Valid() {
		findings = append(findings, domain.ValidationFinding{
			Code: "PRV-006", Field: "notice_consent_dependency",
			Message: "notice_consent_dependency must be explicitly set to REQUIRED, NOT_REQUIRED, or CONDITIONAL — never left implicit",
		})
	}

	// Gate 8: DPIA/TIA requirement resolved by policy or marked review-required (§32: PRV-016)
	if v.DPIATIAStatus == "" || !domain.DPIATIAStatus(v.DPIATIAStatus).Valid() {
		findings = append(findings, domain.ValidationFinding{
			Code: "PRV-016", Field: "dpia_tia_status",
			Message: "dpia_tia_status must be resolved (RESOLVED, NOT_REQUIRED) or marked REVIEW_REQUIRED",
		})
	}

	return findings
}

func (h *Handler) transitionAction(w http.ResponseWriter, r *http.Request, activityID, versionID, actionType string, from, to domain.ActivityVersionStatus, eventType string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, actionType) {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	if !domain.ValidActivityTransition(from, to) {
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
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, actionType, idemKey, reqHash, http.StatusOK, updated)
}

func (h *Handler) SubmitActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")
	h.transitionAction(w, r, activityID, versionID, PrivacyActivitySubmit, domain.ActivityStatusValidated, domain.ActivityStatusSubmitted, "privacy.processing_activity.submitted")
}

func (h *Handler) SubmitLatestActivity(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	latest, ok := h.resolveLatestVersion(w, r, activityID)
	if !ok {
		return
	}
	h.transitionAction(w, r, activityID, latest.ActivityVersionID, PrivacyActivitySubmit, domain.ActivityStatusValidated, domain.ActivityStatusSubmitted, "privacy.processing_activity.submitted")
}

func (h *Handler) ApproveActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")
	h.approveActivityVersionInternal(w, r, activityID, versionID)
}

func (h *Handler) ApproveLatestActivity(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	latest, ok := h.resolveLatestVersion(w, r, activityID)
	if !ok {
		return
	}
	h.approveActivityVersionInternal(w, r, activityID, latest.ActivityVersionID)
}

func (h *Handler) approveActivityVersionInternal(w http.ResponseWriter, r *http.Request, activityID, versionID string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyActivityApprove) {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	// Segregation of Duties (Maker-Checker, §18): Maker cannot approve their own activity version.
	existing, err := h.store.FindActivityVersion(r.Context(), activityID, versionID)
	if err != nil {
		if errors.Is(err, domain.ErrActivityVersionNotFound) {
			writeError(w, http.StatusNotFound, "processing activity version not found")
			return
		}
		h.log.Error("ApproveActivity: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if existing.CreatedByPrincipalID != "" && existing.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "maker cannot approve their own processing activity version (segregation of duties)")
		return
	}

	updated, err := h.store.TransitionActivity(r.Context(), activityID, versionID, domain.ActivityStatusSubmitted, domain.ActivityStatusApproved)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrActivityVersionNotFound):
			writeError(w, http.StatusNotFound, "processing activity version not found")
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "version is not in the required SUBMITTED state")
		default:
			h.log.Error("ApproveActivity: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.processing_activity.approved", EntityID: updated.ActivityID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "ApproveActivity", idemKey, reqHash, http.StatusOK, updated)
}

func (h *Handler) SuspendActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")
	h.transitionAction(w, r, activityID, versionID, PrivacyActivitySuspend, domain.ActivityStatusActive, domain.ActivityStatusSuspended, "privacy.processing_activity.suspended")
}

func (h *Handler) SuspendLatestActivity(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	latest, ok := h.resolveLatestVersion(w, r, activityID)
	if !ok {
		return
	}
	h.transitionAction(w, r, activityID, latest.ActivityVersionID, PrivacyActivitySuspend, domain.ActivityStatusActive, domain.ActivityStatusSuspended, "privacy.processing_activity.suspended")
}

func (h *Handler) ResumeActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")
	h.transitionAction(w, r, activityID, versionID, PrivacyActivityResume, domain.ActivityStatusSuspended, domain.ActivityStatusActive, "privacy.processing_activity.resumed")
}

func (h *Handler) ResumeLatestActivity(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	latest, ok := h.resolveLatestVersion(w, r, activityID)
	if !ok {
		return
	}
	h.transitionAction(w, r, activityID, latest.ActivityVersionID, PrivacyActivityResume, domain.ActivityStatusSuspended, domain.ActivityStatusActive, "privacy.processing_activity.resumed")
}

func (h *Handler) RetireActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")
	h.retireActivityVersionInternal(w, r, activityID, versionID)
}

func (h *Handler) RetireLatestActivity(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	latest, ok := h.resolveLatestVersion(w, r, activityID)
	if !ok {
		return
	}
	h.retireActivityVersionInternal(w, r, activityID, latest.ActivityVersionID)
}

func (h *Handler) retireActivityVersionInternal(w http.ResponseWriter, r *http.Request, activityID, versionID string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyActivityRetire) {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
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
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "RetireActivity", idemKey, reqHash, http.StatusOK, updated)
}

func (h *Handler) RejectActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")
	h.rejectActivityVersionInternal(w, r, activityID, versionID)
}

func (h *Handler) RejectLatestActivity(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	latest, ok := h.resolveLatestVersion(w, r, activityID)
	if !ok {
		return
	}
	h.rejectActivityVersionInternal(w, r, activityID, latest.ActivityVersionID)
}

func (h *Handler) rejectActivityVersionInternal(w http.ResponseWriter, r *http.Request, activityID, versionID string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.RejectActivityRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyActivityReject) {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	// Segregation of Duties (Maker-Checker, §18): Maker cannot reject their own activity version.
	existing, err := h.store.FindActivityVersion(r.Context(), activityID, versionID)
	if err != nil {
		if errors.Is(err, domain.ErrActivityVersionNotFound) {
			writeError(w, http.StatusNotFound, "processing activity version not found")
			return
		}
		h.log.Error("RejectActivityVersion: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if existing.CreatedByPrincipalID != "" && existing.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "maker cannot reject their own processing activity version (segregation of duties)")
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
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "RejectActivity", idemKey, reqHash, http.StatusOK, updated)
}

func (h *Handler) ActivateActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	versionID := chi.URLParam(r, "versionID")
	h.activateActivityVersionInternal(w, r, activityID, versionID)
}

func (h *Handler) ActivateLatestActivity(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	latest, ok := h.resolveLatestVersion(w, r, activityID)
	if !ok {
		return
	}
	h.activateActivityVersionInternal(w, r, activityID, latest.ActivityVersionID)
}

func (h *Handler) activateActivityVersionInternal(w http.ResponseWriter, r *http.Request, activityID, versionID string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.ActivateActivityRequest
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	effectiveFrom := time.Now().UTC()
	if req.EffectiveFrom != nil {
		effectiveFrom = *req.EffectiveFrom
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyActivityActivate) {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
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

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.processing_activity.activated", EntityID: updated.ActivityID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "ActivateActivity", idemKey, reqHash, http.StatusOK, updated)
}

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
