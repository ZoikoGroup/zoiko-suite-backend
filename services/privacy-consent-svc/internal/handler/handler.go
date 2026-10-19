// Package handler exposes privacy-consent-svc's REST API — PRV-02.
//
// Every mutating route runs the same pre-flight, in this order:
//
//  1. X-Principal-Id          -> 401 AUTHENTICATION_FAILED
//  2. X-Tenant-Id             -> 401 TENANT_SCOPE_REQUIRED
//  3. X-Correlation-ID        -> 400 CORRELATION_ID_REQUIRED
//  4. authorization-svc grant on the caller's verified tenant -> 403 / 503 (fail closed)
//  5. Idempotency-Key         -> 400 IDEMPOTENCY_KEY_REQUIRED, then the replay lookup
//     (a lookup failure refuses the request, 503)
//  6. body parsing and validation -> 400 / 422
//  7. resolve the resource -> 404
//
// Body validation therefore never precedes authentication or authorization,
// and authorization precedes every lookup, so an unauthorized caller cannot
// probe which ids exist and a replayed key can never bypass authorization.
// Reads run principal -> tenant -> authorization -> parameter validation ->
// lookup.
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
	// PrivacyNoticePresentationRecord gates recording a presentation receipt:
	// evidence that a notice version was shown to a subject, not a change to
	// the notice itself.
	PrivacyNoticePresentationRecord = "PRIVACY_NOTICE_PRESENTATION_RECORD"

	// PrivacyConsentRead/PrivacyPreferenceRead gate the two reads that
	// answer "what did this subject assert". GetNotice stays ungated: a
	// published notice is, by its own documented purpose, content meant to
	// be shown to the subject before any session exists — but it is still
	// tenant-scoped.
	PrivacyConsentRead    = "PRIVACY_CONSENT_READ"
	PrivacyPreferenceRead = "PRIVACY_PREFERENCE_READ"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

const maxBodyBytes = 1 << 20

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// PurposeChecker is the real cross-service dependency on PRV-01. The caller's
// verified principal is forwarded so PRV-01 can authenticate and authorize
// the read (the grant itself is provisioned in authorization-svc).
type PurposeChecker interface {
	IsPublished(ctx context.Context, tenantID, principalID, purposeID string) (bool, error)
}

// Handler holds no event publisher: every event is enqueued by the STORE,
// inside the same transaction as the evidence row it reports (internal/outbox).
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

// ── responses ────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorResponse carries the stable §32-style ErrorCode on every refusal.
type errorResponse struct {
	Error     string `json:"error"`
	ErrorCode string `json:"error_code,omitempty"`
}

// writeError falls back to the status-derived default code.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeCoded(w, status, codeForStatus(status), msg)
}

func writeCoded(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResponse{Error: msg, ErrorCode: code})
}

// codeForStatus is the fallback when no more specific code applies.
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

// ── pre-flight ───────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if principalID == "" {
		writeCoded(w, http.StatusUnauthorized, domain.ErrCodeAuthenticationFailed, domain.ErrIdentityMissing.Error())
		return "", false
	}
	return principalID, true
}

// requireTenant returns the caller's verified tenant, or refuses. A request
// with no X-Tenant-Id is never treated as platform scope; the platform plane
// must be asked for explicitly with the sentinel id.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := strings.TrimSpace(svcmiddleware.TenantFromContext(r.Context()))
	if tenantID == "" {
		writeCoded(w, http.StatusUnauthorized, domain.ErrCodeTenantScopeRequired, domain.ErrTenantScopeRequired.Error())
		return "", false
	}
	return tenantID, true
}

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

// callContext is the verified identity of one request plus its idempotency
// claim (mutating routes).
type callContext struct {
	Principal   string
	Tenant      string
	Correlation string
	Key         string
	Hash        string
	Body        []byte
}

func requestHash(r *http.Request, body []byte) string {
	h := sha256.New()
	h.Write([]byte(r.Method))
	h.Write([]byte{0})
	h.Write([]byte(r.URL.Path))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// begin runs pre-flight steps 1-5 for a mutating route and reads (but does not
// parse) the body. handled=false/ok=false means a response was written —
// including the replay of an already-seen key.
func (h *Handler) begin(w http.ResponseWriter, r *http.Request, actionType string) (*callContext, bool) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	tenant, ok := h.requireTenant(w, r)
	if !ok {
		return nil, false
	}
	corr, ok := h.requireCorrelationID(w, r)
	if !ok {
		return nil, false
	}
	if !h.authorize(w, r, principal, tenant, actionType) {
		return nil, false
	}

	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeIdempotencyRequired, domain.ErrIdempotencyKeyRequired.Error())
		return nil, false
	}
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read request body")
			return nil, false
		}
		body = b
	}
	cc := &callContext{Principal: principal, Tenant: tenant, Correlation: corr, Key: key, Hash: requestHash(r, body), Body: body}

	// Replay lookup. Fail CLOSED: carrying on without it would execute a
	// request that may be a replay.
	existing, err := h.store.GetIdempotency(r.Context(), tenant, principal, key)
	if err != nil {
		h.log.Error("idempotency lookup failed — refusing the request", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable, "idempotency store unavailable")
		return nil, false
	}
	if existing != nil {
		writeReplay(w, existing, cc.Hash)
		return nil, false
	}
	return cc, true
}

// writeReplay returns the stored original response, or 409 when the key was
// used with a different request.
func writeReplay(w http.ResponseWriter, existing *domain.IdempotencyRecord, hash string) {
	if existing.RequestHash != hash {
		writeCoded(w, http.StatusConflict, domain.ErrCodeIdempotencyMismatch, domain.ErrIdempotencyConflict.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotency-Replay", "true")
	w.WriteHeader(existing.ResponseCode)
	_, _ = w.Write(existing.ResponseBody)
}

// execute runs op and the idempotency claim in ONE transaction: the claim, the
// evidence row(s), the outbox event and the stored replay response all commit
// or none does. Concurrent identical requests execute exactly once. When the
// key was claimed meanwhile, the stored record is returned instead.
func (h *Handler) execute(r *http.Request, cc *callContext, endpoint string, status int, op func(ctx context.Context) (any, error)) (payload any, existing *domain.IdempotencyRecord, err error) {
	claim := domain.IdempotencyRecord{
		Key: cc.Key, TenantID: cc.Tenant, PrincipalID: cc.Principal, Endpoint: endpoint, RequestHash: cc.Hash,
	}
	existing, _, payload, err = h.store.RunIdempotent(r.Context(), claim, func(ctx context.Context) (int, any, error) {
		v, e := op(ctx)
		return status, v, e
	})
	return payload, existing, err
}

// finish writes the first-execution response, or the replay when a concurrent
// request won the claim.
func (h *Handler) finish(w http.ResponseWriter, cc *callContext, existing *domain.IdempotencyRecord, status int, payload any) {
	if existing != nil {
		writeReplay(w, existing, cc.Hash)
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to marshal response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// storeFailure maps an unexpected store error to 503.
func (h *Handler) storeFailure(w http.ResponseWriter, op string, err error) {
	h.log.Error(op+": store unavailable", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "store unavailable")
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
		h.storeFailure(w, "resolveLatestNoticeVersion", err)
		return nil, false
	}
	return latest, true
}

// scopeMismatch refuses a body tenant_id that disagrees with the verified one.
func scopeMismatch(w http.ResponseWriter, bodyTenant, verified string) bool {
	if bodyTenant != "" && bodyTenant != verified {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeTenantScopeMismatch, domain.ErrTenantScopeMismatch.Error())
		return true
	}
	return false
}

// ── notices ──────────────────────────────────────────────────────────────────

func (h *Handler) CreateNotice(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.begin(w, r, PrivacyNoticeCreate)
	if !ok {
		return
	}
	var req domain.CreateNoticeRequest
	if err := json.Unmarshal(cc.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Locale == "" || req.Audience == "" || req.ContentHash == "" {
		writeError(w, http.StatusBadRequest, "locale, audience and content_hash are required")
		return
	}
	if scopeMismatch(w, req.TenantID, cc.Tenant) {
		return
	}

	payload, existing, err := h.execute(r, cc, "CreateNotice", http.StatusCreated, func(ctx context.Context) (any, error) {
		_, version, err := h.store.CreateNotice(ctx, cc.Tenant, req, cc.Principal)
		return version, err
	})
	if err != nil {
		h.storeFailure(w, "CreateNotice", err)
		return
	}
	h.finish(w, cc, existing, http.StatusCreated, payload)
}

func (h *Handler) CreateNoticeVersion(w http.ResponseWriter, r *http.Request) {
	noticeID := chi.URLParam(r, "noticeID")
	cc, ok := h.begin(w, r, PrivacyNoticeCreate)
	if !ok {
		return
	}
	var req domain.CreateNoticeVersionRequest
	if err := json.Unmarshal(cc.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ParentVersionID == "" || req.Locale == "" || req.Audience == "" || req.ContentHash == "" {
		writeError(w, http.StatusBadRequest, "parent_version_id, locale, audience and content_hash are required")
		return
	}

	payload, existing, err := h.execute(r, cc, "CreateNoticeVersion", http.StatusCreated, func(ctx context.Context) (any, error) {
		return h.store.CreateNoticeVersion(ctx, noticeID, req, cc.Principal)
	})
	if err != nil {
		if errors.Is(err, domain.ErrNoticeVersionNotFound) {
			writeError(w, http.StatusNotFound, "parent notice version not found")
			return
		}
		h.storeFailure(w, "CreateNoticeVersion", err)
		return
	}
	h.finish(w, cc, existing, http.StatusCreated, payload)
}

func (h *Handler) ApproveNoticeVersion(w http.ResponseWriter, r *http.Request) {
	h.approveNoticeVersionInternal(w, r, chi.URLParam(r, "noticeID"), chi.URLParam(r, "versionID"))
}

func (h *Handler) ApproveLatestNotice(w http.ResponseWriter, r *http.Request) {
	// versionID "" = "the latest": resolved only AFTER authentication and
	// authorization, so an unauthorized caller learns nothing about whether
	// the notice exists.
	h.approveNoticeVersionInternal(w, r, chi.URLParam(r, "noticeID"), "")
}

func (h *Handler) approveNoticeVersionInternal(w http.ResponseWriter, r *http.Request, noticeID, versionID string) {
	cc, ok := h.begin(w, r, PrivacyNoticeApprove)
	if !ok {
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
		h.storeFailure(w, "ApproveNoticeVersion", err)
		return
	}
	// Segregation of Duties (§18): the maker cannot approve their own version.
	if existing.CreatedByPrincipalID == cc.Principal {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeSoDDenied, "maker cannot approve own notice version (Segregation of Duties)")
		return
	}

	payload, replay, err := h.execute(r, cc, "ApproveNoticeVersion", http.StatusOK, func(ctx context.Context) (any, error) {
		return h.store.ApproveNoticeVersion(ctx, noticeID, versionID, cc.Principal)
	})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidNoticeTransition) {
			writeError(w, http.StatusConflict, "version is not in the required DRAFT state")
			return
		}
		h.storeFailure(w, "ApproveNoticeVersion", err)
		return
	}
	h.finish(w, cc, replay, http.StatusOK, payload)
}

func (h *Handler) PublishNoticeVersion(w http.ResponseWriter, r *http.Request) {
	h.publishNoticeVersionInternal(w, r, chi.URLParam(r, "noticeID"), chi.URLParam(r, "versionID"))
}

func (h *Handler) PublishLatestNotice(w http.ResponseWriter, r *http.Request) {
	h.publishNoticeVersionInternal(w, r, chi.URLParam(r, "noticeID"), "") // see ApproveLatestNotice
}

func (h *Handler) publishNoticeVersionInternal(w http.ResponseWriter, r *http.Request, noticeID, versionID string) {
	cc, ok := h.begin(w, r, PrivacyNoticePublish)
	if !ok {
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
		h.storeFailure(w, "PublishNoticeVersion", err)
		return
	}
	// Segregation of Duties (§18): the maker cannot publish their own version,
	// and the approver cannot also publish it.
	if existing.CreatedByPrincipalID == cc.Principal {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeSoDDenied, "maker cannot publish own notice version (Segregation of Duties)")
		return
	}
	if existing.ApprovedByPrincipalID != nil && *existing.ApprovedByPrincipalID == cc.Principal {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeSoDDenied, "approver cannot also publish notice version (Segregation of Duties)")
		return
	}

	// Publisher evidence AND the privacy.notice.published event are written
	// together with the idempotency claim — one transaction.
	payload, replay, err := h.execute(r, cc, "PublishNoticeVersion", http.StatusOK, func(ctx context.Context) (any, error) {
		return h.store.PublishNoticeVersion(ctx, noticeID, versionID, cc.Principal, cc.Correlation)
	})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidNoticeTransition) {
			writeError(w, http.StatusConflict, "version is not in the required APPROVED state")
			return
		}
		h.storeFailure(w, "PublishNoticeVersion", err)
		return
	}
	h.finish(w, cc, replay, http.StatusOK, payload)
}

func (h *Handler) WithdrawNoticeVersion(w http.ResponseWriter, r *http.Request) {
	h.withdrawNoticeVersionInternal(w, r, chi.URLParam(r, "noticeID"), chi.URLParam(r, "versionID"))
}

func (h *Handler) WithdrawLatestNotice(w http.ResponseWriter, r *http.Request) {
	h.withdrawNoticeVersionInternal(w, r, chi.URLParam(r, "noticeID"), "") // see ApproveLatestNotice
}

func (h *Handler) withdrawNoticeVersionInternal(w http.ResponseWriter, r *http.Request, noticeID, versionID string) {
	cc, ok := h.begin(w, r, PrivacyNoticeWithdraw)
	if !ok {
		return
	}
	if versionID == "" {
		latest, ok := h.resolveLatestNoticeVersion(w, r, noticeID)
		if !ok {
			return
		}
		versionID = latest.NoticeVersionID
	}
	payload, replay, err := h.execute(r, cc, "WithdrawNoticeVersion", http.StatusOK, func(ctx context.Context) (any, error) {
		return h.store.WithdrawNoticeVersion(ctx, noticeID, versionID)
	})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidNoticeTransition) {
			writeError(w, http.StatusConflict, "version is not in the required PUBLISHED state")
			return
		}
		h.storeFailure(w, "WithdrawNoticeVersion", err)
		return
	}
	h.finish(w, cc, replay, http.StatusOK, payload)
}

// GetNotice stays reachable without a principal: a published notice is, by
// its own documented purpose, content meant to be shown to a subject before
// any session exists (§11 Figure 5). It is still tenant-scoped.
func (h *Handler) GetNotice(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	version, err := h.store.ResolveNoticeAsOf(r.Context(), chi.URLParam(r, "noticeID"), parseAsOf(r))
	if err != nil {
		if errors.Is(err, domain.ErrNoticeNotFound) {
			writeError(w, http.StatusNotFound, "notice not found")
			return
		}
		h.storeFailure(w, "GetNotice", err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}

func (h *Handler) RecordPresentation(w http.ResponseWriter, r *http.Request) {
	h.recordPresentationInternal(w, r, chi.URLParam(r, "noticeID"), chi.URLParam(r, "versionID"))
}

func (h *Handler) RecordPresentationLatest(w http.ResponseWriter, r *http.Request) {
	// The canonical route binds to the notice's PUBLISHED version, not its
	// latest-created one (often a DRAFT successor). "" tells
	// recordPresentationInternal to resolve it AFTER authentication and
	// authorization.
	h.recordPresentationInternal(w, r, chi.URLParam(r, "noticeID"), "")
}

func (h *Handler) recordPresentationInternal(w http.ResponseWriter, r *http.Request, noticeID, versionID string) {
	cc, ok := h.begin(w, r, PrivacyNoticePresentationRecord)
	if !ok {
		return
	}
	var req domain.RecordPresentationRequest
	if err := json.Unmarshal(cc.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SubjectRef == "" || req.Channel == "" {
		writeError(w, http.StatusBadRequest, "subject_ref and channel are required")
		return
	}

	if versionID == "" {
		published, err := h.store.FindPublishedNoticeVersion(r.Context(), noticeID)
		if err != nil {
			if errors.Is(err, domain.ErrNoticeVersionInvalid) {
				writeCoded(w, http.StatusUnprocessableEntity, domain.ErrCodeNoticeVersionInvalid, domain.ErrNoticeVersionInvalid.Error())
				return
			}
			h.storeFailure(w, "RecordPresentation", err)
			return
		}
		versionID = published.NoticeVersionID
	}

	payload, replay, err := h.execute(r, cc, "RecordPresentation", http.StatusCreated, func(ctx context.Context) (any, error) {
		return h.store.RecordPresentation(ctx, cc.Tenant, noticeID, versionID, req)
	})
	if err != nil {
		if errors.Is(err, domain.ErrNoticeVersionInvalid) {
			writeCoded(w, http.StatusUnprocessableEntity, domain.ErrCodeNoticeVersionInvalid, domain.ErrNoticeVersionInvalid.Error())
			return
		}
		h.storeFailure(w, "RecordPresentation", err)
		return
	}
	h.finish(w, cc, replay, http.StatusCreated, payload)
}

// ── consent ──────────────────────────────────────────────────────────────────

// RecordConsent handles POST /privacy/consents. purpose_id is validated
// against a REAL call to privacy-purpose-registry-svc. Supports proxy
// verification (§11.1), affirmative-action evidence (§10.1), offline-import
// provenance and replay deduplication (§28, PRV-N04).
func (h *Handler) RecordConsent(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.begin(w, r, PrivacyConsentRecord)
	if !ok {
		return
	}
	var req domain.RecordConsentRequest
	if err := json.Unmarshal(cc.Body, &req); err != nil {
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

	// Offline / imported consent (§11.1): source, capture method, original
	// evidence reference and migration provenance are all required, and
	// imported_at is the consent's own original time (never a fabricated
	// current one). Online consent is unaffected.
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
	if scopeMismatch(w, req.TenantID, cc.Tenant) {
		return
	}

	published, err := h.purposes.IsPublished(r.Context(), cc.Tenant, cc.Principal, req.PurposeID)
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

	// The ConsentReceipt (and its ConsentRequest evidence), the
	// privacy.consent.changed event and the idempotency claim are one
	// transaction.
	payload, existing, err := h.execute(r, cc, "RecordConsent", http.StatusCreated, func(ctx context.Context) (any, error) {
		return h.store.RecordConsent(ctx, cc.Tenant, req, cc.Principal, cc.Correlation)
	})
	if err != nil {
		if errors.Is(err, domain.ErrNoticeVersionInvalid) {
			writeCoded(w, http.StatusUnprocessableEntity, domain.ErrCodeNoticeVersionInvalid, domain.ErrNoticeVersionInvalid.Error())
			return
		}
		h.storeFailure(w, "RecordConsent", err)
		return
	}
	h.finish(w, cc, existing, http.StatusCreated, payload)
}

// ownedReceipt resolves a consent receipt for withdraw/invalidate AFTER
// authorization. A receipt the caller's tenant does not own answers 404, never
// 403 — a distinct forbidden would confirm the id exists.
func (h *Handler) ownedReceipt(w http.ResponseWriter, r *http.Request, op, receiptID, tenant string) bool {
	existing, err := h.store.FindConsentReceipt(r.Context(), receiptID)
	if err != nil {
		if errors.Is(err, domain.ErrConsentReceiptNotFound) {
			writeError(w, http.StatusNotFound, "consent receipt not found")
			return false
		}
		h.storeFailure(w, op, err)
		return false
	}
	if existing.TenantID == nil || *existing.TenantID != tenant {
		writeError(w, http.StatusNotFound, "consent receipt not found")
		return false
	}
	return true
}

// WithdrawConsent handles POST /privacy/consents/{consentReceiptID}/withdraw.
// PRV-I10: never deletes the original receipt. PRV-I11: affects future
// resolution only. PRV-N05: a replayed withdrawal returns the original.
func (h *Handler) WithdrawConsent(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "consentReceiptID")
	cc, ok := h.begin(w, r, PrivacyConsentWithdraw)
	if !ok {
		return
	}
	var req domain.WithdrawConsentRequest
	if err := json.Unmarshal(cc.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Channel == "" {
		writeError(w, http.StatusBadRequest, "channel is required")
		return
	}
	if !h.ownedReceipt(w, r, "WithdrawConsent", receiptID, cc.Tenant) {
		return
	}

	payload, existing, err := h.execute(r, cc, "WithdrawConsent", http.StatusOK, func(ctx context.Context) (any, error) {
		return h.store.WithdrawConsent(ctx, receiptID, req.Channel, cc.Principal, cc.Correlation)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConsentReceiptNotFound):
			writeError(w, http.StatusNotFound, "consent receipt not found")
		case errors.Is(err, domain.ErrAlreadyWithdrawn):
			writeCoded(w, http.StatusConflict, domain.ErrCodeAlreadyWithdrawn, "consent receipt is already withdrawn")
		default:
			h.storeFailure(w, "WithdrawConsent", err)
		}
		return
	}
	h.finish(w, cc, existing, http.StatusOK, payload)
}

// InvalidateConsent handles POST /privacy/consents/{consentReceiptID}/invalidate
// — the platform-initiated counterpart to WithdrawConsent.
func (h *Handler) InvalidateConsent(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "consentReceiptID")
	cc, ok := h.begin(w, r, PrivacyConsentInvalidate)
	if !ok {
		return
	}
	var req domain.InvalidateConsentRequest
	if err := json.Unmarshal(cc.Body, &req); err != nil {
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
	if !h.ownedReceipt(w, r, "InvalidateConsent", receiptID, cc.Tenant) {
		return
	}

	payload, existing, err := h.execute(r, cc, "InvalidateConsent", http.StatusOK, func(ctx context.Context) (any, error) {
		return h.store.InvalidateConsent(ctx, receiptID, req.Reason, req.Channel, cc.Principal, cc.Correlation)
	})
	if err != nil {
		if errors.Is(err, domain.ErrConsentReceiptNotFound) {
			writeError(w, http.StatusNotFound, "consent receipt not found")
			return
		}
		h.storeFailure(w, "InvalidateConsent", err)
		return
	}
	h.finish(w, cc, existing, http.StatusOK, payload)
}

// beginRead runs principal -> tenant -> authorization for a read route.
func (h *Handler) beginRead(w http.ResponseWriter, r *http.Request, actionType string) (principal, tenant string, ok bool) {
	principal, ok = h.requirePrincipal(w, r)
	if !ok {
		return "", "", false
	}
	tenant, ok = h.requireTenant(w, r)
	if !ok {
		return "", "", false
	}
	if !h.authorize(w, r, principal, tenant, actionType) {
		return "", "", false
	}
	return principal, tenant, true
}

// GetConsentStatus answers "what did this subject assert about this purpose"
// — the single most sensitive read this service exposes: principal, verified
// tenant and an explicit grant, all decided before the parameters are even
// validated.
func (h *Handler) GetConsentStatus(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.beginRead(w, r, PrivacyConsentRead); !ok {
		return
	}
	subjectRef := r.URL.Query().Get("subject_ref")
	purposeID := r.URL.Query().Get("purpose_id")
	if subjectRef == "" || purposeID == "" {
		writeError(w, http.StatusBadRequest, "subject_ref and purpose_id query parameters are required")
		return
	}
	resolution, err := h.store.ResolveConsentStatus(r.Context(), subjectRef, purposeID)
	if err != nil {
		h.storeFailure(w, "GetConsentStatus", err)
		return
	}
	writeJSON(w, http.StatusOK, resolution)
}

// ── preferences ──────────────────────────────────────────────────────────────

func (h *Handler) SetPreference(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.begin(w, r, PrivacyPreferenceSet)
	if !ok {
		return
	}
	var req domain.SetPreferenceRequest
	if err := json.Unmarshal(cc.Body, &req); err != nil {
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
	if req.EffectiveAt != nil && req.EffectiveAt.IsZero() {
		writeError(w, http.StatusBadRequest, "effective_at must be a valid RFC 3339 instant")
		return
	}
	if scopeMismatch(w, req.TenantID, cc.Tenant) {
		return
	}

	payload, existing, err := h.execute(r, cc, "SetPreference", http.StatusCreated, func(ctx context.Context) (any, error) {
		return h.store.SetPreference(ctx, cc.Tenant, req)
	})
	if err != nil {
		h.storeFailure(w, "SetPreference", err)
		return
	}
	h.finish(w, cc, existing, http.StatusCreated, payload)
}

// GetPreference is gated exactly like GetConsentStatus.
func (h *Handler) GetPreference(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.beginRead(w, r, PrivacyPreferenceRead); !ok {
		return
	}
	subjectRef := r.URL.Query().Get("subject_ref")
	channelOrPurpose := r.URL.Query().Get("channel_or_purpose")
	if subjectRef == "" || channelOrPurpose == "" {
		writeError(w, http.StatusBadRequest, "subject_ref and channel_or_purpose query parameters are required")
		return
	}
	p, err := h.store.ResolvePreference(r.Context(), subjectRef, channelOrPurpose)
	if err != nil {
		h.storeFailure(w, "GetPreference", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
