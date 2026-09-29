// Package handler exposes privacy-consent-svc's REST API — PRV-02.
package handler

import (
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

	authzpkg "zoiko.io/privacy-consent-svc/internal/authz"
	"zoiko.io/privacy-consent-svc/internal/domain"
	"zoiko.io/privacy-consent-svc/internal/events"
	svcmiddleware "zoiko.io/privacy-consent-svc/internal/middleware"
	"zoiko.io/privacy-consent-svc/internal/store"
)

const (
	PrivacyNoticeCreate    = "PRIVACY_NOTICE_CREATE"
	PrivacyNoticeApprove   = "PRIVACY_NOTICE_APPROVE"
	PrivacyNoticePublish   = "PRIVACY_NOTICE_PUBLISH"
	PrivacyNoticeWithdraw  = "PRIVACY_NOTICE_WITHDRAW"
	PrivacyConsentRecord   = "PRIVACY_CONSENT_RECORD"
	PrivacyConsentWithdraw = "PRIVACY_CONSENT_WITHDRAW"
	PrivacyPreferenceSet   = "PRIVACY_PREFERENCE_SET"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// PurposeChecker is the real cross-service dependency on PRV-01 — see
// internal/purposeregistry's package doc comment for why this can't be a
// trusted opaque string.
type PurposeChecker interface {
	IsPublished(ctx context.Context, purposeID string) (bool, error)
}

type Handler struct {
	store    store.Store
	pub      events.Publisher
	authz    AuthzChecker
	purposes PurposeChecker
	log      *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, purposes PurposeChecker, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, purposes: purposes, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/privacy/notices", func(r chi.Router) {
		r.Post("/", h.CreateNotice)
		r.Get("/{noticeID}", h.GetNotice)
		r.Post("/{noticeID}/versions", h.CreateNoticeVersion)

		// Canonical latest routes
		r.Post("/{noticeID}/approve", h.ApproveLatestNotice)
		r.Post("/{noticeID}/publish", h.PublishLatestNotice)
		r.Post("/{noticeID}/withdraw", h.WithdrawLatestNotice)
		r.Post("/{noticeID}/presentation-receipts", h.RecordPresentationLatest)

		// Explicit version routes
		r.Post("/{noticeID}/versions/{versionID}/approve", h.ApproveNoticeVersion)
		r.Post("/{noticeID}/versions/{versionID}/publish", h.PublishNoticeVersion)
		r.Post("/{noticeID}/versions/{versionID}/withdraw", h.WithdrawNoticeVersion)
		r.Post("/{noticeID}/versions/{versionID}/presentation-receipts", h.RecordPresentation)
	})

	r.Route("/privacy/consents", func(r chi.Router) {
		r.Post("/", h.RecordConsent)
		r.Get("/", h.GetConsentStatus)
		r.Post("/{consentReceiptID}/withdraw", h.WithdrawConsent)
	})

	r.Route("/privacy/preferences", func(r chi.Router) {
		r.Post("/", h.SetPreference)
		r.Get("/", h.GetPreference)
	})
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

func hashBody(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (h *Handler) checkIdempotency(w http.ResponseWriter, r *http.Request, tenantID, principalID string, body []byte) (*domain.IdempotencyRecord, bool, string, string) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return nil, false, "", ""
	}
	reqHash := hashBody(body)
	existing, err := h.store.GetIdempotency(r.Context(), tenantID, key)
	if err != nil {
		h.log.Warn("idempotency lookup error", zap.Error(err))
		return nil, false, key, reqHash
	}
	if existing != nil {
		if existing.RequestHash != reqHash {
			writeError(w, http.StatusConflict, domain.ErrIdempotencyConflict.Error())
			return existing, true, key, reqHash
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotency-Replay", "true")
		w.WriteHeader(existing.ResponseCode)
		_, _ = w.Write(existing.ResponseBody)
		return existing, true, key, reqHash
	}
	return nil, false, key, reqHash
}

func (h *Handler) writeJSONWithIdempotency(ctx context.Context, w http.ResponseWriter, tenantID, principalID, endpoint, key, reqHash string, status int, v interface{}) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to marshal response")
		return
	}
	if key != "" {
		_ = h.store.SaveIdempotency(ctx, domain.IdempotencyRecord{
			Key:          key,
			TenantID:     tenantID,
			Endpoint:     endpoint,
			RequestHash:  reqHash,
			ResponseCode: status,
			ResponseBody: body,
			CreatedAt:    time.Now().UTC(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

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
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC()
	}
	return time.Now().UTC()
}

func (h *Handler) resolveLatestNoticeVersion(w http.ResponseWriter, r *http.Request, noticeID string) (*domain.NoticeVersion, bool) {
	latest, err := h.store.FindLatestNoticeVersion(r.Context(), noticeID)
	if err != nil {
		if errors.Is(err, domain.ErrNoticeNotFound) || errors.Is(err, domain.ErrNoticeVersionNotFound) {
			writeError(w, http.StatusNotFound, "notice not found")
			return nil, false
		}
		h.log.Error("resolveLatestNoticeVersion: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return latest, true
}

// ── notices ──────────────────────────────────────────────────────────────────

func (h *Handler) CreateNotice(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req domain.CreateNoticeRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Locale == "" || req.Audience == "" || req.ContentHash == "" {
		writeError(w, http.StatusBadRequest, "locale, audience and content_hash are required")
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
	if !h.authorize(w, r, principalID, tenantID, PrivacyNoticeCreate) {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	_, version, err := h.store.CreateNotice(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.log.Error("CreateNotice: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "CreateNotice", idemKey, reqHash, http.StatusCreated, version)
}

func (h *Handler) CreateNoticeVersion(w http.ResponseWriter, r *http.Request) {
	noticeID := chi.URLParam(r, "noticeID")
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req domain.CreateNoticeVersionRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ParentVersionID == "" || req.Locale == "" || req.Audience == "" || req.ContentHash == "" {
		writeError(w, http.StatusBadRequest, "parent_version_id, locale, audience and content_hash are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyNoticeCreate) {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	version, err := h.store.CreateNoticeVersion(r.Context(), noticeID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrNoticeVersionNotFound) {
			writeError(w, http.StatusNotFound, "parent notice version not found")
			return
		}
		h.log.Error("CreateNoticeVersion: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "CreateNoticeVersion", idemKey, reqHash, http.StatusCreated, version)
}

func (h *Handler) ApproveNoticeVersion(w http.ResponseWriter, r *http.Request) {
	noticeID := chi.URLParam(r, "noticeID")
	versionID := chi.URLParam(r, "versionID")
	h.approveNoticeVersionInternal(w, r, noticeID, versionID)
}

func (h *Handler) ApproveLatestNotice(w http.ResponseWriter, r *http.Request) {
	noticeID := chi.URLParam(r, "noticeID")
	latest, ok := h.resolveLatestNoticeVersion(w, r, noticeID)
	if !ok {
		return
	}
	h.approveNoticeVersionInternal(w, r, noticeID, latest.NoticeVersionID)
}

func (h *Handler) approveNoticeVersionInternal(w http.ResponseWriter, r *http.Request, noticeID, versionID string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyNoticeApprove) {
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	existing, err := h.store.FindNoticeVersion(r.Context(), noticeID, versionID)
	if err != nil {
		if errors.Is(err, domain.ErrNoticeVersionNotFound) {
			writeError(w, http.StatusNotFound, "notice version not found")
			return
		}
		h.log.Error("ApproveNoticeVersion: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	// Segregation of Duties (§18): Maker cannot self-approve own notice version
	if existing.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "maker cannot approve own notice version (Segregation of Duties)")
		return
	}

	version, err := h.store.ApproveNoticeVersion(r.Context(), noticeID, versionID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidNoticeTransition) {
			writeError(w, http.StatusConflict, "version is not in the required DRAFT state")
			return
		}
		h.log.Error("ApproveNoticeVersion: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "ApproveNoticeVersion", idemKey, reqHash, http.StatusOK, version)
}

func (h *Handler) PublishNoticeVersion(w http.ResponseWriter, r *http.Request) {
	noticeID := chi.URLParam(r, "noticeID")
	versionID := chi.URLParam(r, "versionID")
	h.publishNoticeVersionInternal(w, r, noticeID, versionID)
}

func (h *Handler) PublishLatestNotice(w http.ResponseWriter, r *http.Request) {
	noticeID := chi.URLParam(r, "noticeID")
	latest, ok := h.resolveLatestNoticeVersion(w, r, noticeID)
	if !ok {
		return
	}
	h.publishNoticeVersionInternal(w, r, noticeID, latest.NoticeVersionID)
}

func (h *Handler) publishNoticeVersionInternal(w http.ResponseWriter, r *http.Request, noticeID, versionID string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyNoticePublish) {
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	existing, err := h.store.FindNoticeVersion(r.Context(), noticeID, versionID)
	if err != nil {
		if errors.Is(err, domain.ErrNoticeVersionNotFound) {
			writeError(w, http.StatusNotFound, "notice version not found")
			return
		}
		h.log.Error("PublishNoticeVersion: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	// Segregation of Duties (§18): Maker cannot publish own notice version
	if existing.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "maker cannot publish own notice version (Segregation of Duties)")
		return
	}
	// Checker role separation: Approver cannot also publish
	if existing.ApprovedByPrincipalID != nil && *existing.ApprovedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "approver cannot also publish notice version (Segregation of Duties)")
		return
	}

	version, err := h.store.PublishNoticeVersion(r.Context(), noticeID, versionID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidNoticeTransition) {
			writeError(w, http.StatusConflict, "version is not in the required APPROVED state")
			return
		}
		h.log.Error("PublishNoticeVersion: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.notice.published", EntityID: version.NoticeID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: version,
	})
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "PublishNoticeVersion", idemKey, reqHash, http.StatusOK, version)
}

func (h *Handler) WithdrawNoticeVersion(w http.ResponseWriter, r *http.Request) {
	noticeID := chi.URLParam(r, "noticeID")
	versionID := chi.URLParam(r, "versionID")
	h.withdrawNoticeVersionInternal(w, r, noticeID, versionID)
}

func (h *Handler) WithdrawLatestNotice(w http.ResponseWriter, r *http.Request) {
	noticeID := chi.URLParam(r, "noticeID")
	latest, ok := h.resolveLatestNoticeVersion(w, r, noticeID)
	if !ok {
		return
	}
	h.withdrawNoticeVersionInternal(w, r, noticeID, latest.NoticeVersionID)
}

func (h *Handler) withdrawNoticeVersionInternal(w http.ResponseWriter, r *http.Request, noticeID, versionID string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyNoticeWithdraw) {
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	version, err := h.store.WithdrawNoticeVersion(r.Context(), noticeID, versionID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidNoticeTransition) {
			writeError(w, http.StatusConflict, "version is not in the required PUBLISHED state")
			return
		}
		h.log.Error("WithdrawNoticeVersion: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "WithdrawNoticeVersion", idemKey, reqHash, http.StatusOK, version)
}

func (h *Handler) GetNotice(w http.ResponseWriter, r *http.Request) {
	noticeID := chi.URLParam(r, "noticeID")
	version, err := h.store.ResolveNoticeAsOf(r.Context(), noticeID, parseAsOf(r))
	if err != nil {
		if errors.Is(err, domain.ErrNoticeNotFound) {
			writeError(w, http.StatusNotFound, "notice not found")
			return
		}
		h.log.Error("GetNotice: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, version)
}

func (h *Handler) RecordPresentation(w http.ResponseWriter, r *http.Request) {
	noticeID := chi.URLParam(r, "noticeID")
	versionID := chi.URLParam(r, "versionID")
	h.recordPresentationInternal(w, r, noticeID, versionID)
}

func (h *Handler) RecordPresentationLatest(w http.ResponseWriter, r *http.Request) {
	noticeID := chi.URLParam(r, "noticeID")
	latest, ok := h.resolveLatestNoticeVersion(w, r, noticeID)
	if !ok {
		return
	}
	h.recordPresentationInternal(w, r, noticeID, latest.NoticeVersionID)
}

func (h *Handler) recordPresentationInternal(w http.ResponseWriter, r *http.Request, noticeID, versionID string) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req domain.RecordPresentationRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SubjectRef == "" || req.Channel == "" {
		writeError(w, http.StatusBadRequest, "subject_ref and channel are required")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	principalID := r.Header.Get("X-Principal-Id")
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	receipt, err := h.store.RecordPresentation(r.Context(), verifiedTenant, noticeID, versionID, req)
	if err != nil {
		if errors.Is(err, domain.ErrNoticeVersionNotFound) {
			writeError(w, http.StatusNotFound, "notice version not found")
			return
		}
		h.log.Error("RecordPresentation: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "RecordPresentation", idemKey, reqHash, http.StatusCreated, receipt)
}

// ── consent ──────────────────────────────────────────────────────────────────

// RecordConsent handles POST /privacy/consents. purpose_id is validated
// against a REAL call to privacy-purpose-registry-svc. Supports proxy
// verification (§11.1), affirmative-action evidence (§10.1), and
// replay deduplication (§28, PRV-N04).
func (h *Handler) RecordConsent(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req domain.RecordConsentRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SubjectRef == "" || req.PurposeID == "" || req.CaptureChannel == "" {
		writeError(w, http.StatusBadRequest, "subject_ref, purpose_id and capture_channel are required")
		return
	}
	if !domain.ConsentAction(req.Action).Valid() {
		writeError(w, http.StatusBadRequest, "action must be GRANTED or DENIED")
		return
	}

	// Proxy / Authorized Representative validation (§11.1)
	if req.IsProxy {
		if req.RepresentativeSubjectRef == "" || req.RepresentativeAuthorityRef == "" {
			writeError(w, http.StatusBadRequest, domain.ErrRepresentativeAuthorityRequired.Error())
			return
		}
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
	if !h.authorize(w, r, principalID, tenantID, PrivacyConsentRecord) {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	published, err := h.purposes.IsPublished(r.Context(), req.PurposeID)
	if err != nil {
		h.log.Error("RecordConsent: purpose registry unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "purpose registry unavailable")
		return
	}
	if !published {
		writeError(w, http.StatusUnprocessableEntity, "PRV-001: purpose_id is not a registered, published purpose")
		return
	}

	receipt, err := h.store.RecordConsent(r.Context(), tenantID, req, principalID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.log.Error("RecordConsent: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.consent.changed", EntityID: receipt.SubjectRef, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: receipt,
	})
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "RecordConsent", idemKey, reqHash, http.StatusCreated, receipt)
}

// WithdrawConsent handles POST /privacy/consents/{consentReceiptID}/withdraw.
// PRV-I10: never deletes the original receipt. PRV-I11: affects future
// resolution only. PRV-N05: replayed withdrawal returns original withdrawal.
func (h *Handler) WithdrawConsent(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "consentReceiptID")
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req domain.WithdrawConsentRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Channel == "" {
		writeError(w, http.StatusBadRequest, "channel is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.FindConsentReceipt(r.Context(), receiptID)
	if err != nil {
		if errors.Is(err, domain.ErrConsentReceiptNotFound) {
			writeError(w, http.StatusNotFound, "consent receipt not found")
			return
		}
		h.log.Error("WithdrawConsent: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	tenantID := ""
	if existing.TenantID != nil {
		tenantID = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyConsentWithdraw) {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	withdrawal, err := h.store.WithdrawConsent(r.Context(), receiptID, req.Channel, principalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConsentReceiptNotFound):
			writeError(w, http.StatusNotFound, "consent receipt not found")
		case errors.Is(err, domain.ErrAlreadyWithdrawn):
			writeError(w, http.StatusConflict, "consent receipt is already withdrawn")
		default:
			h.log.Error("WithdrawConsent: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.consent.changed", EntityID: existing.SubjectRef, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: withdrawal,
	})
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "WithdrawConsent", idemKey, reqHash, http.StatusOK, withdrawal)
}

func (h *Handler) GetConsentStatus(w http.ResponseWriter, r *http.Request) {
	subjectRef := r.URL.Query().Get("subject_ref")
	purposeID := r.URL.Query().Get("purpose_id")
	if subjectRef == "" || purposeID == "" {
		writeError(w, http.StatusBadRequest, "subject_ref and purpose_id query parameters are required")
		return
	}

	resolution, err := h.store.ResolveConsentStatus(r.Context(), subjectRef, purposeID)
	if err != nil {
		h.log.Error("GetConsentStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, resolution)
}

// ── preferences ──────────────────────────────────────────────────────────────

func (h *Handler) SetPreference(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req domain.SetPreferenceRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SubjectRef == "" || req.ChannelOrPurpose == "" {
		writeError(w, http.StatusBadRequest, "subject_ref and channel_or_purpose are required")
		return
	}
	if !domain.PreferenceValue(req.Value).Valid() {
		writeError(w, http.StatusBadRequest, "value must be one of ENABLED, DISABLED, UNSET, NOT_APPLICABLE")
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
	if !h.authorize(w, r, principalID, tenantID, PrivacyPreferenceSet) {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	p, err := h.store.SetPreference(r.Context(), tenantID, req)
	if err != nil {
		h.log.Error("SetPreference: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "SetPreference", idemKey, reqHash, http.StatusCreated, p)
}

func (h *Handler) GetPreference(w http.ResponseWriter, r *http.Request) {
	subjectRef := r.URL.Query().Get("subject_ref")
	channelOrPurpose := r.URL.Query().Get("channel_or_purpose")
	if subjectRef == "" || channelOrPurpose == "" {
		writeError(w, http.StatusBadRequest, "subject_ref and channel_or_purpose query parameters are required")
		return
	}

	p, err := h.store.ResolvePreference(r.Context(), subjectRef, channelOrPurpose)
	if err != nil {
		h.log.Error("GetPreference: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, p)
}
