package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"
	authzpkg "zoiko.io/key-management-svc/internal/authz"
	kmscrypto "zoiko.io/key-management-svc/internal/crypto"
	"zoiko.io/key-management-svc/internal/domain"
	svcenvelope "zoiko.io/key-management-svc/internal/envelope"
	"zoiko.io/key-management-svc/internal/siem"
	"zoiko.io/key-management-svc/internal/store"
)

// requireTenant returns the gateway-verified tenant, or refuses the
// request. It replaces a getTenant helper that substituted the literal
// string "default-tenant" when X-Tenant-ID was absent.
//
// That default was worse than a missing check, and worse here than in most
// services. A missing check makes a request fail; a fabricated tenant makes
// it SUCCEED, into one synthetic bucket shared by every header-less
// caller. The store is correctly scoped on every method, so it faithfully
// enforced that shared bucket: header-less callers could list each other's
// customer-key metadata, and — because RotateKey and DisableKey are scoped
// the same way — rotate or DISABLE each other's keys. Disabling a key is a
// denial of service on whatever that key protects, so the fabricated
// identity turned a correctly-written store into a shared control plane
// over key material.
//
// Returns "" and writes 401 rather than falling back to anything. Note the
// header is read as X-Tenant-ID and set elsewhere as X-Tenant-Id: Go
// canonicalises header keys, so both forms match the same value. That is
// not a bug, and normalising the spelling here would be a cosmetic change
// to a security path.
func requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := r.Header.Get("X-Tenant-ID")
	if tenantID == "" {
		apicontract.WriteProblem(w, r, apicontract.Problem{
			Status:    http.StatusUnauthorized,
			Code:      "missing_tenant_scope",
			Detail:    "X-Tenant-ID is required — the gateway sets it from a verified identity envelope",
			Retryable: false,
		})
		return "", false
	}
	return tenantID, true
}

// Action constants passed to authorization-svc as action_type.
//
// Split by consequence. KEY_DISABLE is its own privilege because disabling a
// customer-managed key is the destructive operation here: whatever that key
// protects stops being decryptable. Today this service is metadata-only —
// tracker row 82 records that it is never actually used to encrypt or
// decrypt anything — so a disable currently just flips a status field. That
// is exactly why the boundary belongs here NOW rather than later: the moment
// these records drive real KMS operations, an unauthorized disable becomes
// unrecoverable data loss, and nobody re-derives the authorization model
// while wiring up crypto.
//
// KEY_ROTATE is separate from KEY_REGISTER for the same reason as in
// mtls-management-svc: advancing an existing key's version is a different
// privilege from declaring a new key.
//
// TOKEN_DETOKENIZE is split from everything else for the same reason
// KEY_DISABLE is split from KEY_ROTATE, and more so: it is the one action in
// this service that hands a caller a raw S3 Restricted value (a bank account
// number). SEC-T15 requires detokenization to carry "additional scoped
// approval", so holding any other privilege here — even TOKEN_CREATE, which
// put the value in — must not imply it. It is a v1 of that control, not the
// full one: a single elevated grant checked at authorization-svc, not a
// maker-checker approval per request (SPEC_DEVIATIONS.md, section 3).
//
// TOKEN_CREATE guards tokenize. Writing a value into the vault discloses
// nothing, but it is a material write, and the doctrine is that material
// writes are authorized — so it is a grant of its own, not implied by
// detokenize and not implying it.
const (
	KeyRegister     = "KEY_REGISTER"
	KeyRead         = "KEY_READ"
	KeyRotate       = "KEY_ROTATE"
	KeyDisable      = "KEY_DISABLE"
	TokenCreate     = "TOKEN_CREATE"
	TokenDetokenize = "TOKEN_DETOKENIZE"
)

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store  store.Store
	siem   *siem.Client
	authz  AuthzChecker
	logger *zap.Logger

	// vault and cipher back /v1/tokenize and /v1/detokenize. Set by
	// WithVault; while unset those two routes refuse with 503 rather than
	// tokenizing into nowhere.
	vault  store.Vault
	cipher *kmscrypto.Cipher
}

// requirePrincipal returns the gateway-verified principal, or refuses the
// request. Nothing read X-Principal-Id on any route before now, so this
// service had a verified tenant (Priority 1b) but no notion of WHO acted.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		h.errJSON(w, r, http.StatusUnauthorized,
			"X-Principal-Id is required — the gateway sets it from a verified identity envelope")
		return "", false
	}
	return principalID, true
}

// authorize asks authorization-svc whether this principal may perform
// actionType within legalEntityID, and fails CLOSED.
//
// Every route here can carry the full guard, unlike siem-integration-svc
// where two routes had to stay tenant-only because five other services call
// them with tenant-only headers. Verified for this service: nothing calls it
// over HTTP — no KEY_MANAGEMENT_SERVICE_URL anywhere, no /v1/keys caller
// outside its own tree — so requiring a principal breaks no existing
// integration. That check is the reason this is 5 of 5 and siem was 3 of 5.
//
// legalEntityID is taken from the key ROW on reads and mutations by id,
// never from anything the caller supplied.
//
// That "nothing calls it over HTTP" premise changed with /v1/tokenize and
// /v1/detokenize: banking-connector-svc and payee-banking-identity-svc are
// built to call them (behind TOKENIZATION_ENFORCED, off by default). They
// send a principal, so the full guard still applies to every route.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		h.writeAuthzRefusal(w, r, err)
		return false
	}
	return true
}

// writeAuthzRefusal answers a failed CheckAllowed: 403 for a denial, 503 —
// fail closed — for anything else. Split out of authorize so detokenize can
// record the outcome before answering.
func (h *Handler) writeAuthzRefusal(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
		h.errJSON(w, r, http.StatusForbidden, "not authorized to perform this action")
		return
	}
	h.logger.Error("authorization check failed", zap.Error(err))
	h.errJSON(w, r, http.StatusServiceUnavailable, "authorization service unavailable")
}

func New(s store.Store, siemClient *siem.Client, az AuthzChecker, l *zap.Logger) *Handler {
	return &Handler{store: s, siem: siemClient, authz: az, logger: l}
}

// WithVault enables the token vault routes: v holds ciphertext, c encrypts
// and decrypts it. Kept off New's signature so every existing caller of New
// keeps compiling and keeps its exact behaviour.
func (h *Handler) WithVault(v store.Vault, c *kmscrypto.Cipher) *Handler {
	h.vault, h.cipher = v, c
	return h
}

func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, chimw.Logger, chimw.Recoverer)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer so a refusal is still traced, and ahead of every handler so no
	// request reaches business logic without a resolved tenant, actor,
	// correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	// Refusals rendered as RFC 9457 problem+json, like every other error here.
	r.Use(apicontract.EnvelopeRefusals)
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "key-management-svc"})
	})
	r.Route("/v1/keys", func(r chi.Router) {
		r.Post("/", h.RegisterKey)
		r.Get("/", h.ListKeys)
		r.Get("/{id}", h.GetKey)
		r.Post("/{id}/rotate", h.RotateKey)
		r.Post("/{id}/disable", h.DisableKey)
	})
	// The token vault is a different resource from /v1/keys. Both routes are
	// POST: a token or a raw value never travels in a URL, where proxies and
	// access logs would keep it.
	r.Post("/v1/tokenize", h.Tokenize)
	r.Post("/v1/detokenize", h.Detokenize)
	return r
}

func (h *Handler) RegisterKey(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	var req domain.RegisterKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, r, 400, "invalid body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, r, 400, err.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, KeyRegister) {
		return
	}
	key := &domain.CustomerKey{
		LegalEntityID:  req.LegalEntityID,
		KeyAlias:       req.KeyAlias,
		KeyModel:       req.KeyModel,
		KeyProvider:    req.KeyProvider,
		ExternalKeyARN: req.ExternalKeyARN,
	}
	if err := h.store.CreateKey(r.Context(), tenantID, key); err != nil {
		h.errJSON(w, r, 500, "failed to register key")
		return
	}
	h.okJSON(w, r, 201, key)
}

func (h *Handler) GetKey(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	key, err := h.store.GetKeyByID(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.writeKeyStoreErr(w, r, "get key", err)
		return
	}
	if !h.authorize(w, r, principalID, key.LegalEntityID, KeyRead) {
		return
	}
	h.okJSON(w, r, 200, key)
}

func (h *Handler) ListKeys(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// legal_entity_id is now REQUIRED. It is the authorization scope for a
	// listing, and an optional scope parameter is a scope that disables
	// itself — the same shape as the self-disabling filters found across the
	// six connector services. Omitting it previously returned every key in
	// the tenant across all legal entities.
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		h.errJSON(w, r, http.StatusBadRequest,
			"legal_entity_id query parameter is required — it is the authorization scope for this listing")
		return
	}
	if !h.authorize(w, r, principalID, legalEntityID, KeyRead) {
		return
	}
	// The error used to be discarded, so a database outage answered 200 with
	// an empty list — indistinguishable from "this entity has no keys".
	keys, err := h.store.ListKeys(r.Context(), tenantID, legalEntityID)
	if err != nil {
		h.writeKeyStoreErr(w, r, "list keys", err)
		return
	}
	if keys == nil {
		keys = []domain.CustomerKey{}
	}
	h.okJSON(w, r, 200, map[string]interface{}{"data": keys, "count": len(keys)})
}

func (h *Handler) RotateKey(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	// Read before rotating. store.RotateKey both mutates and returns the
	// row, so authorizing against its result would mean the rotation had
	// already happened — the authorization decision has to precede the
	// mutation, which means fetching the row purely to learn whose it is.
	existing, err := h.store.GetKeyByID(r.Context(), tenantID, id)
	if err != nil {
		h.writeKeyStoreErr(w, r, "rotate key (read)", err)
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, KeyRotate) {
		return
	}

	key, err := h.store.RotateKey(r.Context(), tenantID, id)
	if err != nil {
		h.writeKeyStoreErr(w, r, "rotate key", err)
		return
	}
	// Doc 05 §13.2 names "secret retrieval" and key lifecycle events as
	// required SIEM signals — rotation is the one BYOK/HYOK lifecycle event
	// this service can report with certainty (it happened, right now, to
	// this specific key), unlike a bare GetKey which returns only metadata.
	h.siem.Stream(r.Context(), tenantID, "key.rotated", siem.SeverityMedium, "Key rotated: "+id)
	h.okJSON(w, r, 200, key)
}

func (h *Handler) DisableKey(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	// Same ordering constraint as rotate, and it matters more here: this is
	// the destructive route. store.DisableKey returns only an error, so
	// without a prior read there is no way to know whose key was disabled
	// until after disabling it.
	existing, err := h.store.GetKeyByID(r.Context(), tenantID, id)
	if err != nil {
		h.writeKeyStoreErr(w, r, "disable key (read)", err)
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, KeyDisable) {
		return
	}

	if err := h.store.DisableKey(r.Context(), tenantID, id); err != nil {
		h.writeKeyStoreErr(w, r, "disable key", err)
		return
	}
	h.siem.Stream(r.Context(), tenantID, "key.disabled", siem.SeverityHigh, "Key disabled: "+id)
	h.okJSON(w, r, 200, map[string]string{"message": "key disabled"})
}

// writeKeyStoreErr answers a failed key-store call on the get, list, rotate
// and disable routes.
//
// Only store.ErrKeyNotFound is a 404. Before Postgres these routes answered
// every store error with 404 (and ListKeys discarded its error, answering an
// empty 200), which was harmless while the store was an in-process map that
// could not fail. With a database it meant an outage looked exactly like
// "this key does not exist" — during incident response, the moment someone
// is trying to disable a key. A store failure is now 503 and retryable, and
// logged; a non-UUID tenant, which can never own a row, is 400.
func (h *Handler) writeKeyStoreErr(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, store.ErrKeyNotFound):
		h.errJSON(w, r, http.StatusNotFound, "key not found")
	case errors.Is(err, store.ErrInvalidTenant):
		h.errJSON(w, r, http.StatusBadRequest, "X-Tenant-ID must be a UUID")
	default:
		h.logger.Error("key store unavailable", zap.String("op", op), zap.Error(err))
		h.errJSON(w, r, http.StatusServiceUnavailable, "key store unavailable")
	}
}

func (h *Handler) okJSON(w http.ResponseWriter, r *http.Request, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// errJSON answers a refusal that names no code of its own. The code is the
// status's class (apicontract.CodeForStatus), never a blanket internal_error,
// so a caller has a stable value to match on (ZS-API-001 §12).
func (h *Handler) errJSON(w http.ResponseWriter, r *http.Request, code int, msg string) {
	apicontract.WriteProblem(w, r, apicontract.Problem{
		Status:    code,
		Code:      apicontract.CodeForStatus(code),
		Detail:    msg,
		Retryable: code >= 500,
	})
}
