package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/key-management-svc/internal/domain"
	"zoiko.io/key-management-svc/internal/store"
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
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "key-management-svc"})
	})
	r.Route("/v1/keys", func(r chi.Router) {
		r.Post("/", h.RegisterKey)
		r.Get("/", h.ListKeys)
		r.Get("/{id}", h.GetKey)
		r.Post("/{id}/rotate", h.RotateKey)
		r.Post("/{id}/disable", h.DisableKey)
	})
	return r
}

func (h *Handler) RegisterKey(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	var req domain.RegisterKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, 400, "invalid body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, 400, err.Error())
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
		h.errJSON(w, 500, "failed to register key")
		return
	}
	h.okJSON(w, 201, key)
}

func (h *Handler) GetKey(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	key, err := h.store.GetKeyByID(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.errJSON(w, 404, "key not found")
		return
	}
	h.okJSON(w, 200, key)
}

func (h *Handler) ListKeys(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	keys, _ := h.store.ListKeys(r.Context(), tenantID, r.URL.Query().Get("legal_entity_id"))
	if keys == nil {
		keys = []domain.CustomerKey{}
	}
	h.okJSON(w, 200, map[string]interface{}{"data": keys, "count": len(keys)})
}

func (h *Handler) RotateKey(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	key, err := h.store.RotateKey(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.errJSON(w, 404, "key not found")
		return
	}
	h.okJSON(w, 200, key)
}

func (h *Handler) DisableKey(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	if err := h.store.DisableKey(r.Context(), tenantID, chi.URLParam(r, "id")); err != nil {
		h.errJSON(w, 404, "key not found")
		return
	}
	h.okJSON(w, 200, map[string]string{"message": "key disabled"})
}

func (h *Handler) okJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func (h *Handler) errJSON(w http.ResponseWriter, code int, msg string) {
	h.okJSON(w, code, map[string]string{"error": msg})
}
