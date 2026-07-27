package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/mtls-management-svc/internal/domain"
	"zoiko.io/mtls-management-svc/internal/store"
)

type ctxKey string

const tenantKey ctxKey = "tenant_id"

func getTenant(r *http.Request) string {
	t := r.Header.Get("X-Tenant-ID")
	if t == "" {
		return "default-tenant"
	}
	return t
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
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "mtls-management-svc"})
	})
	r.Route("/v1/mtls", func(r chi.Router) {
		r.Post("/certificates", h.ProvisionCert)
		r.Get("/certificates", h.ListCerts)
		r.Get("/certificates/{id}", h.GetCert)
		r.Post("/certificates/{id}/rotate", h.RotateCert)
		r.Delete("/certificates/{id}", h.RevokeCert)
		r.Post("/policies", h.CreatePolicy)
		r.Get("/policies", h.ListPolicies)
	})
	return r
}

func (h *Handler) ProvisionCert(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	var req domain.ProvisionCertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, 400, "invalid body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, 400, err.Error())
		return
	}
	cert := domain.GenerateCertificate(&req, tenantID)
	if err := h.store.CreateCert(r.Context(), tenantID, cert); err != nil {
		h.errJSON(w, 500, "failed to provision certificate")
		return
	}
	h.okJSON(w, 201, cert)
}

func (h *Handler) GetCert(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	cert, err := h.store.GetCertByID(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.errJSON(w, 404, "certificate not found")
		return
	}
	h.okJSON(w, 200, cert)
}

func (h *Handler) ListCerts(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	certs, _ := h.store.ListCerts(r.Context(), tenantID, r.URL.Query().Get("legal_entity_id"), r.URL.Query().Get("status"))
	if certs == nil {
		certs = []domain.MtlsCertificate{}
	}
	h.okJSON(w, 200, map[string]interface{}{"data": certs, "count": len(certs)})
}

func (h *Handler) RotateCert(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	cert, err := h.store.RotateCert(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.errJSON(w, 404, "certificate not found")
		return
	}
	h.okJSON(w, 200, cert)
}

func (h *Handler) RevokeCert(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	if err := h.store.RevokeCert(r.Context(), tenantID, chi.URLParam(r, "id")); err != nil {
		h.errJSON(w, 404, "certificate not found")
		return
	}
	h.okJSON(w, 200, map[string]string{"message": "certificate revoked"})
}

func (h *Handler) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	var req domain.CreatePolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PolicyName == "" {
		h.errJSON(w, 400, "policy_name is required")
		return
	}
	pol := &domain.CommunicationPolicy{
		PolicyName:    req.PolicyName,
		SourceService: req.SourceService,
		TargetService: req.TargetService,
		Action:        req.Action,
		RequiresMtls:  req.RequiresMtls,
	}
	_ = h.store.CreatePolicy(r.Context(), tenantID, pol)
	h.okJSON(w, 201, pol)
}

func (h *Handler) ListPolicies(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	pols, _ := h.store.ListPolicies(r.Context(), tenantID)
	if pols == nil {
		pols = []domain.CommunicationPolicy{}
	}
	h.okJSON(w, 200, map[string]interface{}{"data": pols, "count": len(pols)})
}

func (h *Handler) okJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func (h *Handler) errJSON(w http.ResponseWriter, code int, msg string) {
	h.okJSON(w, code, map[string]string{"error": msg})
}
