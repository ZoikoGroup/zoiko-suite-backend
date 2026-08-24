package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/key-management-svc/internal/domain"
	svcenvelope "zoiko.io/key-management-svc/internal/envelope"
	"zoiko.io/key-management-svc/internal/siem"
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
	siem   *siem.Client
	logger *zap.Logger
}

func New(s store.Store, siemClient *siem.Client, l *zap.Logger) *Handler {
	return &Handler{store: s, siem: siemClient, logger: l}
}

func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, chimw.Logger, chimw.Recoverer)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer so a refusal is still traced, and ahead of every handler so no
	// request reaches business logic without a resolved tenant, actor,
	// correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
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
	id := chi.URLParam(r, "id")
	key, err := h.store.RotateKey(r.Context(), tenantID, id)
	if err != nil {
		h.errJSON(w, 404, "key not found")
		return
	}
	// Doc 05 §13.2 names "secret retrieval" and key lifecycle events as
	// required SIEM signals — rotation is the one BYOK/HYOK lifecycle event
	// this service can report with certainty (it happened, right now, to
	// this specific key), unlike a bare GetKey which returns only metadata.
	h.siem.Stream(r.Context(), tenantID, "key.rotated", siem.SeverityMedium, "Key rotated: "+id)
	h.okJSON(w, 200, key)
}

func (h *Handler) DisableKey(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	id := chi.URLParam(r, "id")
	if err := h.store.DisableKey(r.Context(), tenantID, id); err != nil {
		h.errJSON(w, 404, "key not found")
		return
	}
	h.siem.Stream(r.Context(), tenantID, "key.disabled", siem.SeverityHigh, "Key disabled: "+id)
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
