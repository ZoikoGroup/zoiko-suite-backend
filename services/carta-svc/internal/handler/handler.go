package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/carta-svc/internal/domain"
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

type Handler struct {
	store  store.Store
	logger *zap.Logger
}

func New(s store.Store, l *zap.Logger) *Handler { return &Handler{store: s, logger: l} }

func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, chimw.Logger, chimw.Recoverer)
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
	asm, err := h.store.GetAssessmentByID(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.errJSON(w, 404, "assessment not found")
		return
	}
	h.okJSON(w, 200, asm)
}

func (h *Handler) ListAssessments(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	asms, _ := h.store.ListAssessments(r.Context(), tenantID, r.URL.Query().Get("subject_id"))
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
