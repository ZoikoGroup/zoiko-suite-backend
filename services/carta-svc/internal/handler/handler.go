package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	authzpkg "zoiko.io/carta-svc/internal/authz"
	"zoiko.io/carta-svc/internal/domain"
	svcenvelope "zoiko.io/carta-svc/internal/envelope"
	"zoiko.io/carta-svc/internal/store"
)

// requireTenant returns the gateway-verified tenant, or refuses the
// request. It replaces a getTenant helper that substituted the literal
// string "default-tenant" when X-Tenant-ID was absent.
//
// A missing check makes a request fail; a fabricated tenant makes it
// SUCCEED, into one synthetic bucket shared by every header-less caller.
// The store here is correctly written — SaveAssessment stamps the tenant,
// and both reads compare it, with subject_id narrowing WITHIN the tenant
// rather than replacing it — so it enforced the fake tenant faithfully as
// though it were real.
//
// What that shared bucket held is this service's whole point. A
// CartaAssessment records who requested access to what, from which IP, on
// what device (DeviceTrustLevel), at what hour, and what the platform
// decided: ALLOW / STEP_UP_MFA / ISOLATE / DENY. RiskFactors spells the
// reasoning out in plain text ("Untrusted device (trust level 30)",
// "Access request from unknown IP/location").
//
// Since domain.EvaluateAccess is a deterministic scoring function, reading
// another tenant's assessments is close to reading a map of how to pass
// their access checks: which subjects sit on weak devices, which IPs they
// treat as known, where their allow/deny boundary falls, and which lever
// moved any given score. That is a read exposure only — the store has no
// update or delete — but it is the security posture of another tenant's
// workforce.
func requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := r.Header.Get("X-Tenant-ID")
	if tenantID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "missing_tenant_scope",
			"message": "X-Tenant-ID is required — the gateway sets it from a verified identity envelope",
		})
		return "", false
	}
	return tenantID, true
}

// Action constants passed to authorization-svc as action_type.
const (
	CartaAssessmentRead = "CARTA_ASSESSMENT_READ"
)

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store  store.Store
	authz  AuthzChecker
	logger *zap.Logger
}

func New(s store.Store, az AuthzChecker, l *zap.Logger) *Handler {
	return &Handler{store: s, authz: az, logger: l}
}

// ── Why POST /v1/carta/evaluate has NO principal guard ──────────────────
//
// Two of this service's three routes require a verified principal and an
// authorization decision. /evaluate does not, and that is not an oversight
// to be tidied up later — a guard there would be circular.
//
// gateway-auth-svc calls POST /v1/carta/evaluate (and only that route — its
// client has no reference to /assessments) with X-Tenant-ID and nothing
// else. It passes the principal it is asking ABOUT as context.subject_id in
// the request body, not as a caller identity.
//
// That is the whole point of the endpoint. It is called DURING
// authentication, to score whether an access request should be allowed,
// stepped up to MFA, isolated or denied. Requiring the subject to prove it
// is authorized before the platform will decide whether it is trustworthy
// inverts the dependency: the answer would be needed to ask the question.
//
// This is the same shape as siem-integration-svc's /stream (row 84d) —
// gateway-auth-svc streaming an authentication FAILURE has no authenticated
// principal either — and the correct control is the same: SERVICE identity
// via mutual TLS, which mtls-management-svc already issues. Until that is
// wired, /evaluate stays tenant-scoped only.
//
// Residual exposure, named rather than dismissed: a caller holding a tenant
// header can submit fabricated evaluation requests, which both pollutes the
// tenant's assessment history and lets an attacker probe the scoring
// function to learn where the ALLOW boundary sits. Logged with row 84d
// rather than left implicit.
//
// TestEvaluateStillWorksWithoutPrincipal pins the current behaviour, so a
// future "consistency" change fails loudly instead of breaking the
// platform's authentication path.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		h.errJSON(w, http.StatusUnauthorized,
			"X-Principal-Id is required — the gateway sets it from a verified identity envelope")
		return "", false
	}
	return principalID, true
}

// authorize asks authorization-svc whether this principal may perform
// actionType within legalEntityID, and fails CLOSED.
//
// One action, not several: both guarded routes are reads of the same
// resource, and splitting READ from LIST would create two grants that no
// policy would ever hold separately.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			h.errJSON(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		h.logger.Error("authorization check failed", zap.Error(err))
		h.errJSON(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}

func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, chimw.Logger, chimw.Recoverer)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer so a refusal is still traced, and ahead of every handler so no
	// request reaches business logic without a resolved tenant, actor,
	// correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	//
	// POST /v1/carta/evaluate is exempt, and the exemption is not a loosening.
	// gateway-auth-svc calls it DURING authentication, to score the attempt, so
	// the caller has no envelope yet — requiring one would mean needing a
	// verified identity in order to establish a verified identity. The same
	// circularity identity-context-svc's /v1/context/resolve documents, and the
	// route is tenant-scoped and writes nothing a caller can read back.
	// TestEvaluateStillWorksWithoutPrincipal pins it.
	envelopePolicy := svcenvelope.ServicePolicy()
	envelopePolicy.ExemptPaths = []string{"/v1/carta/evaluate"}
	r.Use(svcenvelope.Middleware(envelopePolicy, svcenvelope.DefaultReporter()))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "carta-svc"})
	})
	r.Route("/v1/carta", func(r chi.Router) {
		r.Post("/evaluate", h.EvaluateAccess)
		r.Get("/assessments", h.ListAssessments)
		r.Get("/assessments/{id}", h.GetAssessment)
	})
	return r
}

func (h *Handler) EvaluateAccess(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	var req domain.EvaluateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, 400, "invalid body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, 400, err.Error())
		return
	}
	asm := domain.EvaluateAccess(&req, tenantID)
	if err := h.store.SaveAssessment(r.Context(), tenantID, asm); err != nil {
		h.errJSON(w, 500, "failed to save assessment")
		return
	}
	h.okJSON(w, 201, asm)
}

func (h *Handler) GetAssessment(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	asm, err := h.store.GetAssessmentByID(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.errJSON(w, 404, "assessment not found")
		return
	}
	// Authorized against the assessment row's own legal entity, never a
	// caller-supplied scope. The store is already tenant-scoped, so another
	// tenant's assessment is a 404 above.
	if !h.authorize(w, r, principalID, asm.LegalEntityID, CartaAssessmentRead) {
		return
	}
	h.okJSON(w, 200, asm)
}

func (h *Handler) ListAssessments(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// legal_entity_id is REQUIRED — it is the authorization scope for a
	// listing, and an optional scope parameter is a scope that disables
	// itself. subject_id stays optional and narrows within it.
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		h.errJSON(w, http.StatusBadRequest,
			"legal_entity_id query parameter is required — it is the authorization scope for this listing")
		return
	}
	if !h.authorize(w, r, principalID, legalEntityID, CartaAssessmentRead) {
		return
	}
	asms, _ := h.store.ListAssessments(r.Context(), tenantID, legalEntityID, r.URL.Query().Get("subject_id"))
	if asms == nil {
		asms = []domain.CartaAssessment{}
	}
	h.okJSON(w, 200, map[string]interface{}{"data": asms, "count": len(asms)})
}

func (h *Handler) okJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func (h *Handler) errJSON(w http.ResponseWriter, code int, msg string) {
	h.okJSON(w, code, map[string]string{"error": msg})
}
