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
	tenantID := getTenant(r)
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
	tenantID := getTenant(r)
	asm, err := h.store.GetAssessmentByID(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.errJSON(w, 404, "assessment not found")
		return
	}
	h.okJSON(w, 200, asm)
}

func (h *Handler) ListAssessments(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
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
