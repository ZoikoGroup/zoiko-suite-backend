package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/key-management-svc/internal/domain"
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
	siem   *siem.Client
	logger *zap.Logger
}

func New(s store.Store, siemClient *siem.Client, l *zap.Logger) *Handler {
	return &Handler{store: s, siem: siemClient, logger: l}
}

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
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
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
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	key, err := h.store.GetKeyByID(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.errJSON(w, 404, "key not found")
		return
	}
	h.okJSON(w, 200, key)
}

func (h *Handler) ListKeys(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	keys, _ := h.store.ListKeys(r.Context(), tenantID, r.URL.Query().Get("legal_entity_id"))
	if keys == nil {
		keys = []domain.CustomerKey{}
	}
	h.okJSON(w, 200, map[string]interface{}{"data": keys, "count": len(keys)})
}

func (h *Handler) RotateKey(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
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
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
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
