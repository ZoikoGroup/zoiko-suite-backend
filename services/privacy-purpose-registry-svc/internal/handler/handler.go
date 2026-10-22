// Package handler exposes privacy-purpose-registry-svc's REST API — PRV-01.
//
// Every route — read or write — runs the same pre-flight, in this order:
//
//  1. X-Principal-Id          -> 401 AUTHENTICATION_FAILED
//  2. X-Tenant-Id             -> 401 TENANT_SCOPE_REQUIRED (never "platform by omission")
//  3. authorization-svc grant -> 403 AUTHORIZATION_DENIED / 503 (fail closed)
//  4. mutating only: X-Correlation-ID -> 400 CORRELATION_ID_REQUIRED
//  5. mutating only: Idempotency-Key  -> 400 IDEMPOTENCY_KEY_REQUIRED, then replay
//
// Authentication and authorization strictly precede the idempotent replay, so
// a principal whose grant has been revoked cannot replay a cached success.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-purpose-registry-svc/internal/authz"
	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	svcmiddleware "zoiko.io/privacy-purpose-registry-svc/internal/middleware"
	"zoiko.io/privacy-purpose-registry-svc/internal/store"
)

// Action constants passed to authorization-svc as action_type — SCREAMING_
// SNAKE_CASE, same convention as every other service in this platform.
const (
	PrivacyPurposeCreate  = "PRIVACY_PURPOSE_CREATE"
	PrivacyPurposePublish = "PRIVACY_PURPOSE_PUBLISH"
	PrivacyPurposeRead    = "PRIVACY_PURPOSE_READ"

	PrivacyActivityCreate   = "PRIVACY_ACTIVITY_CREATE"
	PrivacyActivityValidate = "PRIVACY_ACTIVITY_VALIDATE"
	PrivacyActivitySubmit   = "PRIVACY_ACTIVITY_SUBMIT"
	PrivacyActivityApprove  = "PRIVACY_ACTIVITY_APPROVE"
	PrivacyActivityReject   = "PRIVACY_ACTIVITY_REJECT"
	PrivacyActivityActivate = "PRIVACY_ACTIVITY_ACTIVATE"
	PrivacyActivitySuspend  = "PRIVACY_ACTIVITY_SUSPEND"
	PrivacyActivityResume   = "PRIVACY_ACTIVITY_RESUME"
	PrivacyActivityRetire   = "PRIVACY_ACTIVITY_RETIRE"
	PrivacyActivityRead     = "PRIVACY_ACTIVITY_READ"

	// PrivacyRopaRead gates the Article 30 inventory projection separately
	// from single-activity reads: §8.1 limits callers to privacy admins,
	// approved domain owners, the change pipeline and compliance services,
	// and the whole-inventory view is the most sensitive of the reads.
	PrivacyRopaRead = "PRIVACY_ROPA_READ"
)

// maxBodyBytes bounds a request body; registry payloads are small lists of
// identifiers and a sentence of text.
const maxBodyBytes = 1 << 20

// effectiveTimeTolerance is the clock-skew allowance for a caller-supplied
// effective_from on activation — see activateSpec.
const effectiveTimeTolerance = 60 * time.Second

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Handler wires HTTP routes to domain logic and data stores. It holds no
// event publisher: every event is enqueued by the store inside the same
// transaction as the state change (internal/events/outbox.go).
type Handler struct {
	store store.Store
	authz AuthzChecker
	log   *zap.Logger
	now   func() time.Time
}

func New(st store.Store, az AuthzChecker, log *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, log: log, now: func() time.Time { return time.Now().UTC() }}
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

// ── responses & errors ───────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorResponse is the stable error envelope (§32). error_code is always
// present; findings accompanies an activation refusal so the caller sees every
// failed gate, each with its own §32 code.
type errorResponse struct {
	Error     string                     `json:"error"`
	ErrorCode string                     `json:"error_code"`
	Findings  []domain.ValidationFinding `json:"findings,omitempty"`
}

func writeCoded(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResponse{Error: msg, ErrorCode: code})
}

func (h *Handler) storeUnavailable(w http.ResponseWriter, op string, err error) {
	h.log.Error(op+": store unavailable", zap.Error(err))
	writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable, "store unavailable")
}

// ── pre-flight ───────────────────────────────────────────────────────────────

// callContext is the verified identity and scope of one request.
type callContext struct {
	Principal   string
	Scope       string // the verified X-Tenant-Id; domain.PlatformScopeID for the platform plane
	Platform    bool
	Correlation string // "" on reads
}

// RecordTenant is the tenant a new row is stamped with, and the tenant the
// idempotency record is filed under: "" (SQL NULL) for the platform plane.
func (c *callContext) RecordTenant() string {
	if c.Platform {
		return ""
	}
	return c.Scope
}

// OwnsRecord reports whether this request's scope may mutate a record owned by
// recordTenant (nil = platform record). A tenant may mutate only its own
// records; the platform plane only platform records. Row-level security
// enforces the same rule underneath — this is the 403 that tells the caller why.
func (c *callContext) OwnsRecord(recordTenant *string) bool {
	if c.Platform {
		return recordTenant == nil
	}
	return recordTenant != nil && *recordTenant == c.Scope
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if principalID == "" {
		writeCoded(w, http.StatusUnauthorized, domain.ErrCodeAuthenticationFailed, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// requireTenant refuses a request that carries no X-Tenant-Id. §18.1 makes
// tenant scope mandatory; it used to be read and, if absent, silently treated
// as platform scope, so a caller who sent no tenant at all could read the
// platform's records and write platform-wide ones. Platform scope must now be
// asked for explicitly, by sending domain.PlatformScopeID.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	scope := svcmiddleware.TenantFromContext(r.Context())
	if scope == "" {
		writeCoded(w, http.StatusUnauthorized, domain.ErrCodeTenantScopeRequired,
			"X-Tenant-Id header is required (send the platform scope id to act in the platform plane)")
		return "", false
	}
	return scope, true
}

func (h *Handler) requireCorrelationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	corrID := strings.TrimSpace(r.Header.Get("X-Correlation-ID"))
	if corrID == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeCorrelationRequired, "X-Correlation-ID header is required")
		return "", false
	}
	w.Header().Set("X-Correlation-ID", corrID)
	return corrID, true
}

// authorize asks authorization-svc whether principalID may perform
// actionType within scope (a tenant, or the platform scope id). Fail closed:
// anything other than an explicit GRANTED is a refusal.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, scope, actionType string) bool {
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

// begin runs steps 1-4 of the pre-flight. mutating adds the correlation-ID
// requirement. On false the response has been written.
func (h *Handler) begin(w http.ResponseWriter, r *http.Request, actionType string, mutating bool) (*callContext, bool) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	scope, ok := h.requireTenant(w, r)
	if !ok {
		return nil, false
	}
	if !h.authorize(w, r, principal, scope, actionType) {
		return nil, false
	}
	cc := &callContext{Principal: principal, Scope: scope, Platform: svcmiddleware.IsPlatformScope(r.Context())}
	if mutating {
		corr, ok := h.requireCorrelationID(w, r)
		if !ok {
			return nil, false
		}
		cc.Correlation = corr
	}
	return cc, true
}

func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "failed to read request body")
		return nil, false
	}
	return b, true
}

// ── idempotency (§18.1) ──────────────────────────────────────────────────────

// idempotency enforces the mandatory Idempotency-Key and serves replays. It
// runs AFTER begin(), i.e. after authentication and authorization, so a replay
// is never a way around either. handled=true means a response was written.
//
// The request hash covers method, path and body, so the same key reused on a
// different operation or payload is a 409, never a silent replay of the wrong
// answer. The record is scoped to (tenant-or-platform, principal, key).
func (h *Handler) idempotency(w http.ResponseWriter, r *http.Request, cc *callContext, body []byte) (key, hash string, handled bool) {
	key = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeIdempotencyRequired, "Idempotency-Key header is required on mutating requests")
		return "", "", true
	}
	hash = computeRequestHash(r.Method, r.URL.Path, body)
	rec, err := h.store.GetIdempotency(r.Context(), cc.RecordTenant(), cc.Principal, key)
	if err != nil {
		h.storeUnavailable(w, "GetIdempotency", err)
		return key, hash, true
	}
	if rec != nil {
		writeReplay(w, rec, hash)
		return key, hash, true
	}
	return key, hash, false
}

// execute runs fn and the idempotency claim in ONE transaction: the change, its
// audit row, its outbox event and the stored replay response all commit or none
// does (store.RunIdempotent — every store call fn makes joins the transaction).
// A failed save therefore fails the whole operation instead of being logged and
// ignored, and concurrent identical requests execute exactly once. When the key
// was claimed meanwhile, the stored record is returned instead.
func (h *Handler) execute(r *http.Request, cc *callContext, op, key, hash string, status int, fn func(ctx context.Context) (any, error)) (payload any, existing *domain.IdempotencyRecord, err error) {
	var tID *string
	if t := cc.RecordTenant(); t != "" {
		tID = &t
	}
	claim := domain.IdempotencyRecord{IdempotencyKey: key, TenantID: tID, PrincipalID: cc.Principal, Operation: op, RequestHash: hash}
	existing, _, payload, err = h.store.RunIdempotent(r.Context(), claim, func(ctx context.Context) (int, any, error) {
		v, e := fn(ctx)
		return status, v, e
	})
	return payload, existing, err
}

// finish writes the first-execution response, or the replay when a concurrent
// request won the claim.
func (h *Handler) finish(w http.ResponseWriter, existing *domain.IdempotencyRecord, hash string, status int, payload interface{}) {
	if existing != nil {
		writeReplay(w, existing, hash)
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		writeCoded(w, http.StatusInternalServerError, domain.ErrCodeInternal, "failed to serialize response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeReplay returns the stored original response, or 409 when the key was
// used with a different request.
func writeReplay(w http.ResponseWriter, rec *domain.IdempotencyRecord, hash string) {
	if rec.RequestHash != hash {
		writeCoded(w, http.StatusConflict, domain.ErrCodeIdempotencyMismatch, "idempotency key reused with different request payload")
		return
	}
	w.Header().Set("Idempotency-Replay", "true")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rec.ResponseStatus)
	_, _ = w.Write(rec.ResponseBody)
}

// ── small helpers ────────────────────────────────────────────────────────────

// scopeMismatch refuses a body tenant_id that disagrees with the verified
// X-Tenant-Id. An empty body tenant_id is fine; the verified scope governs.
func scopeMismatch(w http.ResponseWriter, bodyTenantID string, cc *callContext) bool {
	if bodyTenantID != "" && bodyTenantID != cc.Scope {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeTenantScopeMismatch, "tenant_id does not match the verified X-Tenant-Id")
		return true
	}
	return false
}

// wrongPlane refuses a mutation of a record the caller's scope does not own.
func wrongPlane(w http.ResponseWriter, cc *callContext, recordTenant *string) bool {
	if cc.OwnsRecord(recordTenant) {
		return false
	}
	if recordTenant == nil {
		writeCoded(w, http.StatusForbidden, domain.ErrCodePlatformScope,
			"this is a platform record: it can only be changed from the platform plane (X-Tenant-Id = platform scope id) by a principal holding the platform grant")
	} else {
		writeCoded(w, http.StatusForbidden, domain.ErrCodePlatformScope,
			"this record belongs to a tenant: it cannot be changed from the platform plane or another tenant")
	}
	return true
}

// parseInstants reads as_of (valid time, default now) and known_as_of
// (knowledge time, default now). A value that is present but not RFC 3339 is a
// 400, never a silent fall-back to "now": a historical-evidence query that
// quietly answers about today is worse than one that fails.
func (h *Handler) parseInstants(w http.ResponseWriter, r *http.Request) (asOf, knownAsOf time.Time, ok bool) {
	now := h.now()
	parse := func(name string) (time.Time, bool) {
		raw := r.URL.Query().Get(name)
		if raw == "" {
			return now, true
		}
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeCoded(w, http.StatusBadRequest, domain.ErrCodeInvalidAsOf, name+" must be an RFC 3339 timestamp, e.g. 2026-01-02T15:04:05Z")
			return time.Time{}, false
		}
		return t.UTC(), true
	}
	if asOf, ok = parse("as_of"); !ok {
		return
	}
	if knownAsOf, ok = parse("known_as_of"); !ok {
		return
	}
	return asOf, knownAsOf, true
}

func nonBlank(xs []string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			out = append(out, x)
		}
	}
	return out
}

func hasBlank(xs []string) bool { return len(nonBlank(xs)) != len(xs) }
