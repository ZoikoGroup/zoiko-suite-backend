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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-consent-svc/internal/authz"
	"zoiko.io/privacy-consent-svc/internal/domain"
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
	// PrivacyConsentInvalidate gates the platform-initiated counterpart to
	// withdrawal — its own action, deliberately: a principal who may record
	// or withdraw a subject's own consent should not automatically be able
	// to invalidate ANY subject's grant on the platform's own initiative.
	PrivacyConsentInvalidate = "PRIVACY_CONSENT_INVALIDATE"
	PrivacyPreferenceSet     = "PRIVACY_PREFERENCE_SET"
	// PrivacyNoticePresentationRecord gates recordPresentationInternal —
	// distinct from the four PrivacyNotice* lifecycle actions above, which
	// govern the notice's own draft/approve/publish/withdraw state.
	// Recording a presentation receipt is a separate capability: it is
	// evidence that a notice version was actually shown to a subject, not
	// a change to the notice itself.
	PrivacyNoticePresentationRecord = "PRIVACY_NOTICE_PRESENTATION_RECORD"

	// PrivacyConsentRead/PrivacyPreferenceRead gate the two reads that
	// answer "what did this subject assert". They used to require no
	// principal and no authorization at all — any caller who could reach
	// the service (the port is published directly in local compose) and
	// set an arbitrary X-Tenant-Id header could read any tenant's subject
	// consent/preference state. GetNotice stays ungated: a published
	// notice is, by its own documented purpose, content meant to be shown
	// to the subject before any session exists — but it is still tenant-
	// scoped, see requireTenant on GetNotice.
	PrivacyConsentRead    = "PRIVACY_CONSENT_READ"
	PrivacyPreferenceRead = "PRIVACY_PREFERENCE_READ"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// PurposeChecker is the real cross-service dependency on PRV-01 — see
// internal/purposeregistry's package doc comment for why this can't be a
// trusted opaque string.
type PurposeChecker interface {
	IsPublished(ctx context.Context, tenantID, purposeID string) (bool, error)
}

// Handler no longer holds an events.Publisher. Every event this service
// produces (privacy.notice.published, privacy.consent.changed) is now
// enqueued by the STORE, inside the same transaction as the evidence row
// it reports (internal/outbox) — see RecordConsent/WithdrawConsent/
// InvalidateConsent/publishNoticeVersionInternal's own comments for why
// the old post-commit, fire-and-forget publish call here was the gap:
// a crash or a broker outage between "the write committed" and "Kafka got
// it" lost the event with no trace and no retry.
type Handler struct {
	store    store.Store
	authz    AuthzChecker
	purposes PurposeChecker
	log      *zap.Logger
}

func New(st store.Store, az AuthzChecker, purposes PurposeChecker, log *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, purposes: purposes, log: log}
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
		// InvalidateConsent — the platform-initiated counterpart to
		// withdrawal. See domain.ConsentInvalidation's own doc comment.
		r.Post("/{consentReceiptID}/invalidate", h.InvalidateConsent)
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

// errorResponse carries the stable §32-style ErrorCode on every refusal —
// previously every error from this service was an untyped
// {"error": "..."} string, so a caller could only branch on prose.
type errorResponse struct {
	Error     string `json:"error"`
	ErrorCode string `json:"error_code,omitempty"`
}

// writeError is kept for call sites that have not been given a specific
// code yet; it falls back to the status-derived default. New code should
// prefer writeCoded.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeCoded(w, status, codeForStatus(status), msg)
}

func writeCoded(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResponse{Error: msg, ErrorCode: code})
}

// codeForStatus is the fallback when no more specific code applies — still
// typed and stable, just coarser than a dedicated code.
func codeForStatus(status int) string {
	switch status {
	case http.StatusNotFound:
		return domain.ErrCodeNotFound
	case http.StatusBadRequest:
		return domain.ErrCodeValidationFailed
	case http.StatusUnauthorized:
		return domain.ErrCodeAuthenticationFailed
	case http.StatusForbidden:
		return domain.ErrCodeAuthorizationDenied
	case http.StatusConflict:
		return domain.ErrCodeIdempotencyMismatch
	case http.StatusUnprocessableEntity:
		return domain.ErrCodeInvalidTransition
	case http.StatusServiceUnavailable:
		return domain.ErrCodeStoreUnavailable
	default:
		return domain.ErrCodeInternal
	}
}

func hashBody(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// checkIdempotency enforces §18.1: "All mutating APIs require ... an
// idempotency key." A missing header used to be treated as "proceed
// without replay protection"; it is now a 400, the same posture
// privacy-purpose-registry-svc and tenant-entity-registry-svc already
// take. Every existing call site already does `if handled { return }`
// immediately after calling this, so the mandatory check needed no change
// to any of them beyond this function.
func (h *Handler) checkIdempotency(w http.ResponseWriter, r *http.Request, tenantID, principalID string, body []byte) (*domain.IdempotencyRecord, bool, string, string) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeIdempotencyRequired, domain.ErrIdempotencyKeyRequired.Error())
		return nil, true, "", ""
	}
	reqHash := hashBody(body)
	existing, err := h.store.GetIdempotency(r.Context(), tenantID, key)
	if err != nil {
		// Fail CLOSED. Carrying on without the lookup would execute a request
		// that may be a replay with no replay protection at all — exactly what
		// the mandatory key exists to prevent.
		h.log.Error("idempotency lookup failed — refusing the request", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable, "idempotency store unavailable")
		return nil, true, key, reqHash
	}
	if existing != nil {
		if existing.RequestHash != reqHash {
			writeCoded(w, http.StatusConflict, domain.ErrCodeIdempotencyMismatch, domain.ErrIdempotencyConflict.Error())
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
		if err := h.store.SaveIdempotency(ctx, domain.IdempotencyRecord{
			Key:          key,
			TenantID:     tenantID,
			Endpoint:     endpoint,
			RequestHash:  reqHash,
			ResponseCode: status,
			ResponseBody: body,
			CreatedAt:    time.Now().UTC(),
		}); err != nil {
			// The operation has ALREADY COMMITTED. Reporting success would
			// leave a client that retries after a network error unprotected;
			// hiding it would be worse. So the failure is surfaced, with the
			// committed result attached so the caller can recover without
			// repeating the operation.
			h.log.Error("idempotency record not saved after commit", zap.String("endpoint", endpoint), zap.Error(err))
			writeJSON(w, http.StatusInternalServerError, struct {
				Error     string          `json:"error"`
				ErrorCode string          `json:"error_code"`
				Committed bool            `json:"committed"`
				Result    json.RawMessage `json:"result"`
			}{
				Error:     "the operation was committed but its idempotency record could not be saved; a retry with the same Idempotency-Key will not be recognised as a replay",
				ErrorCode: domain.ErrCodeIdempotencyNotRecorded,
				Committed: true,
				Result:    body,
			})
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeCoded(w, http.StatusUnauthorized, domain.ErrCodeAuthenticationFailed, domain.ErrIdentityMissing.Error())
		return "", false
	}
	return principalID, true
}

// requireTenant returns the caller's verified tenant, or refuses. §18.1
// requires tenant scope to be server-validated on every mutating API; it
// used to be read and, if absent, simply treated as "platform scope" —
// which meant a caller who sent no X-Tenant-Id at all could create a
// platform-wide notice, record a platform-wide consent, or set a
// platform-wide preference with nothing to stop them. A genuinely
// platform-wide record (Notice.TenantID nil, "Zoiko as independent
// controller") is still representable — see the sentinel constant below —
// but it must now be asked for explicitly via X-Tenant-Id, never produced
// by omission.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeCoded(w, http.StatusUnauthorized, domain.ErrCodeTenantScopeRequired, domain.ErrTenantScopeRequired.Error())
		return "", false
	}
	return tenantID, true
}

// requireCorrelationID enforces §18.1's "correlation ID" requirement on
// mutating APIs. It used to be read opportunistically (used if present,
// silently absent otherwise), which left every write's downstream event
// untraceable to the request that caused it whenever a caller omitted the
// header.
func (h *Handler) requireCorrelationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(r.Header.Get("X-Correlation-ID"))
	if id == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeCorrelationRequired, domain.ErrCorrelationIDRequired.Error())
		return "", false
	}
	return id, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, tenantID, actionType string) bool {
	scope := platformScopeID
	if tenantID != "" {
		scope = tenantID
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, scope, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeCoded(w, http.StatusForbidden, domain.ErrCodeAuthorizationDenied, "not authorized to perform this action")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeAuthzUnavailable, "authorization service unavailable")
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

	verifiedTenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if req.TenantID != "" && req.TenantID != verifiedTenant {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeTenantScopeMismatch, domain.ErrTenantScopeMismatch.Error())
		return
	}
	tenantID := verifiedTenant

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyNoticeCreate) {
		return
	}
	if _, ok := h.requireCorrelationID(w, r); !ok {
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
	verifiedTenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyNoticeCreate) {
		return
	}
	if _, ok := h.requireCorrelationID(w, r); !ok {
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
	// versionID "" = "the latest": resolved inside, only AFTER authentication
	// and authorization, so an unauthorized caller learns nothing about whether
	// the notice exists.
	h.approveNoticeVersionInternal(w, r, chi.URLParam(r, "noticeID"), "")
}

func (h *Handler) approveNoticeVersionInternal(w http.ResponseWriter, r *http.Request, noticeID, versionID string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireCorrelationID(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyNoticeApprove) {
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	if versionID == "" {
		latest, ok := h.resolveLatestNoticeVersion(w, r, noticeID)
		if !ok {
			return
		}
		versionID = latest.NoticeVersionID
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
		writeCoded(w, http.StatusForbidden, domain.ErrCodeSoDDenied, "maker cannot approve own notice version (Segregation of Duties)")
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
	h.publishNoticeVersionInternal(w, r, chi.URLParam(r, "noticeID"), "") // see ApproveLatestNotice
}

func (h *Handler) publishNoticeVersionInternal(w http.ResponseWriter, r *http.Request, noticeID, versionID string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyNoticePublish) {
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	if versionID == "" {
		latest, ok := h.resolveLatestNoticeVersion(w, r, noticeID)
		if !ok {
			return
		}
		versionID = latest.NoticeVersionID
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
		writeCoded(w, http.StatusForbidden, domain.ErrCodeSoDDenied, "maker cannot publish own notice version (Segregation of Duties)")
		return
	}
	// Checker role separation: Approver cannot also publish
	if existing.ApprovedByPrincipalID != nil && *existing.ApprovedByPrincipalID == principalID {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeSoDDenied, "approver cannot also publish notice version (Segregation of Duties)")
		return
	}

	// version_status/publisher evidence AND the privacy.notice.published
	// event are written together, inside the store's own transaction — see
	// PgStore.PublishNoticeVersion's doc comment.
	version, err := h.store.PublishNoticeVersion(r.Context(), noticeID, versionID, principalID, correlationID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidNoticeTransition) {
			writeError(w, http.StatusConflict, "version is not in the required APPROVED state")
			return
		}
		h.log.Error("PublishNoticeVersion: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	h.writeJSONWithIdempotency(r.Context(), w, verifiedTenant, principalID, "PublishNoticeVersion", idemKey, reqHash, http.StatusOK, version)
}

func (h *Handler) WithdrawNoticeVersion(w http.ResponseWriter, r *http.Request) {
	noticeID := chi.URLParam(r, "noticeID")
	versionID := chi.URLParam(r, "versionID")
	h.withdrawNoticeVersionInternal(w, r, noticeID, versionID)
}

func (h *Handler) WithdrawLatestNotice(w http.ResponseWriter, r *http.Request) {
	h.withdrawNoticeVersionInternal(w, r, chi.URLParam(r, "noticeID"), "") // see ApproveLatestNotice
}

func (h *Handler) withdrawNoticeVersionInternal(w http.ResponseWriter, r *http.Request, noticeID, versionID string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireCorrelationID(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyNoticeWithdraw) {
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	if versionID == "" {
		latest, ok := h.resolveLatestNoticeVersion(w, r, noticeID)
		if !ok {
			return
		}
		versionID = latest.NoticeVersionID
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

// GetNotice stays reachable without a principal: a published notice is, by
// its own documented purpose, content meant to be shown to a subject
// before any session exists (ORG-style "notice precedes consent" ordering,
// §11 Figure 5: NOTICE PUBLISHED -> PRESENTED comes before any consent
// decision). It is still tenant-scoped — requireTenant — so a notice id
// cannot be read across tenants by a caller who sends no, or a guessed,
// X-Tenant-Id.
func (h *Handler) GetNotice(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
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
	// The canonical route binds to the notice's PUBLISHED version, not its
	// latest-created one (often a DRAFT successor — which a presentation
	// receipt may no longer bind to). "" tells recordPresentationInternal to
	// resolve it AFTER authentication and authorization, so an anonymous
	// caller cannot probe notice ids through the resolution's error.
	h.recordPresentationInternal(w, r, chi.URLParam(r, "noticeID"), "")
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

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, verifiedTenant, PrivacyNoticePresentationRecord) {
		return
	}
	if _, ok := h.requireCorrelationID(w, r); !ok {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	if versionID == "" {
		published, err := h.store.FindPublishedNoticeVersion(r.Context(), noticeID)
		if err != nil {
			if errors.Is(err, domain.ErrNoticeVersionInvalid) {
				writeCoded(w, http.StatusUnprocessableEntity, domain.ErrCodeNoticeVersionInvalid, domain.ErrNoticeVersionInvalid.Error())
				return
			}
			h.log.Error("RecordPresentation: published-version lookup failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
			return
		}
		versionID = published.NoticeVersionID
	}

	receipt, err := h.store.RecordPresentation(r.Context(), verifiedTenant, noticeID, versionID, req)
	if err != nil {
		if errors.Is(err, domain.ErrNoticeVersionInvalid) {
			writeCoded(w, http.StatusUnprocessableEntity, domain.ErrCodeNoticeVersionInvalid, domain.ErrNoticeVersionInvalid.Error())
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

	// Offline / imported consent (§11.1): "require source, capture method,
	// original evidence reference and migration provenance; never fabricate
	// current-platform consent timestamps." All four map to this service's
	// import_* fields, and imported_at is the consent's own original time — it
	// is what stops a migrated grant being stamped with today's date. Online
	// consent is unaffected.
	if req.IsOfflineImport {
		var missing []string
		if strings.TrimSpace(req.ImportSource) == "" {
			missing = append(missing, "import_source")
		}
		if strings.TrimSpace(req.ImportCaptureMethod) == "" {
			missing = append(missing, "import_capture_method")
		}
		if strings.TrimSpace(req.ImportEvidenceRef) == "" {
			missing = append(missing, "import_evidence_ref")
		}
		if req.ImportedAt == nil || req.ImportedAt.IsZero() {
			missing = append(missing, "imported_at")
		}
		if len(missing) > 0 {
			writeCoded(w, http.StatusBadRequest, domain.ErrCodeImportProvenanceRequired,
				"offline/imported consent requires "+strings.Join(missing, ", ")+" (§11.1)")
			return
		}
		if req.ImportedAt.After(time.Now().UTC()) {
			writeCoded(w, http.StatusBadRequest, domain.ErrCodeImportProvenanceRequired,
				"imported_at is the original consent time and cannot be in the future")
			return
		}
	}

	verifiedTenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if req.TenantID != "" && req.TenantID != verifiedTenant {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeTenantScopeMismatch, domain.ErrTenantScopeMismatch.Error())
		return
	}
	tenantID := verifiedTenant

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyConsentRecord) {
		return
	}
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	published, err := h.purposes.IsPublished(r.Context(), tenantID, req.PurposeID)
	if err != nil {
		h.log.Error("RecordConsent: purpose registry unavailable", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeUpstreamUnavailable, "purpose registry unavailable")
		return
	}
	if !published {
		writeCoded(w, http.StatusUnprocessableEntity, domain.ErrCodePurposeNotRegistered, "PRV-001: purpose_id is not a registered, published purpose")
		return
	}

	if strings.TrimSpace(req.NoticeVersionID) == "" {
		writeCoded(w, http.StatusUnprocessableEntity, domain.ErrCodeNoticeVersionInvalid, domain.ErrNoticeVersionInvalid.Error())
		return
	}

	// The ConsentReceipt (and its ConsentRequest evidence, when supplied) are
	// written, and the privacy.consent.changed event enqueued, in one
	// transaction — see PgStore.RecordConsent's own doc comment.
	receipt, err := h.store.RecordConsent(r.Context(), tenantID, req, principalID, correlationID)
	if err != nil {
		if errors.Is(err, domain.ErrNoticeVersionInvalid) {
			writeCoded(w, http.StatusUnprocessableEntity, domain.ErrCodeNoticeVersionInvalid, domain.ErrNoticeVersionInvalid.Error())
			return
		}
		h.log.Error("RecordConsent: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

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
	verifiedTenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}

	// Authorization is decided on the caller's own verified tenant BEFORE the
	// receipt is looked up, so a caller who is not authorized cannot use the
	// 404/200 difference to probe receipt ids.
	tenantID := verifiedTenant
	if !h.authorize(w, r, principalID, tenantID, PrivacyConsentWithdraw) {
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
	// A receipt the caller's tenant does not own answers 404, never 403 — a
	// distinct forbidden would confirm the id exists, and a receipt id is
	// exactly what an attacker would be probing for. Row-level security
	// (strict since migration 000006) already stops FindConsentReceipt from
	// returning another tenant's row; a row with NO tenant is likewise treated
	// as not found rather than as some shared scope. This is the belt to that
	// braces.
	if existing.TenantID == nil || *existing.TenantID != verifiedTenant {
		writeError(w, http.StatusNotFound, "consent receipt not found")
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	// The WithdrawalReceipt and the privacy.consent.changed event are
	// written together — see PgStore.WithdrawConsent's own doc comment.
	withdrawal, err := h.store.WithdrawConsent(r.Context(), receiptID, req.Channel, principalID, correlationID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConsentReceiptNotFound):
			writeError(w, http.StatusNotFound, "consent receipt not found")
		case errors.Is(err, domain.ErrAlreadyWithdrawn):
			writeCoded(w, http.StatusConflict, domain.ErrCodeAlreadyWithdrawn, "consent receipt is already withdrawn")
		default:
			h.log.Error("WithdrawConsent: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "WithdrawConsent", idemKey, reqHash, http.StatusOK, withdrawal)
}

// InvalidateConsent handles POST /privacy/consents/{consentReceiptID}/invalidate
// — the platform-initiated counterpart to WithdrawConsent. See
// domain.ConsentInvalidation's own doc comment for why the two are
// recorded as distinct facts.
func (h *Handler) InvalidateConsent(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "consentReceiptID")
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.InvalidateConsentRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
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
	verifiedTenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}

	// Authorize on the caller's own verified tenant first, then look up — see
	// WithdrawConsent for why both the order and the strict ownership check.
	tenantID := verifiedTenant
	if !h.authorize(w, r, principalID, tenantID, PrivacyConsentInvalidate) {
		return
	}

	existing, err := h.store.FindConsentReceipt(r.Context(), receiptID)
	if err != nil {
		if errors.Is(err, domain.ErrConsentReceiptNotFound) {
			writeError(w, http.StatusNotFound, "consent receipt not found")
			return
		}
		h.log.Error("InvalidateConsent: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if existing.TenantID == nil || *existing.TenantID != verifiedTenant {
		writeError(w, http.StatusNotFound, "consent receipt not found")
		return
	}

	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	inv, err := h.store.InvalidateConsent(r.Context(), receiptID, req.Reason, req.Channel, principalID, correlationID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConsentReceiptNotFound):
			writeError(w, http.StatusNotFound, "consent receipt not found")
		default:
			h.log.Error("InvalidateConsent: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "InvalidateConsent", idemKey, reqHash, http.StatusOK, inv)
}

// GetConsentStatus answers "what did this subject assert about this
// purpose" — the single most sensitive read this service exposes. It used
// to require no principal, no authorization and no verified tenant at
// all: the service's compose definition publishes its port directly, so
// any caller who could reach it, and who set an arbitrary X-Tenant-Id
// header, could read any tenant's subject consent state. It is now gated
// exactly like a mutating route: a verified principal, a verified tenant
// (which is what scopes the underlying RLS-protected query), and an
// explicit grant.
func (h *Handler) GetConsentStatus(w http.ResponseWriter, r *http.Request) {
	subjectRef := r.URL.Query().Get("subject_ref")
	purposeID := r.URL.Query().Get("purpose_id")
	if subjectRef == "" || purposeID == "" {
		writeError(w, http.StatusBadRequest, "subject_ref and purpose_id query parameters are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyConsentRead) {
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

	verifiedTenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if req.TenantID != "" && req.TenantID != verifiedTenant {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeTenantScopeMismatch, domain.ErrTenantScopeMismatch.Error())
		return
	}
	tenantID := verifiedTenant

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyPreferenceSet) {
		return
	}
	if _, ok := h.requireCorrelationID(w, r); !ok {
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

// GetPreference is gated the same way GetConsentStatus now is — see that
// handler's own doc comment for why an unauthenticated, tenant-unscoped
// read of per-subject preference state was the gap.
func (h *Handler) GetPreference(w http.ResponseWriter, r *http.Request) {
	subjectRef := r.URL.Query().Get("subject_ref")
	channelOrPurpose := r.URL.Query().Get("channel_or_purpose")
	if subjectRef == "" || channelOrPurpose == "" {
		writeError(w, http.StatusBadRequest, "subject_ref and channel_or_purpose query parameters are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyPreferenceRead) {
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
