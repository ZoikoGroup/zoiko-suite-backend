package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/siem-integration-svc/internal/domain"
	"zoiko.io/siem-integration-svc/internal/store"
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
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "siem-integration-svc"})
	})
	r.Route("/v1/siem", func(r chi.Router) {
		r.Post("/exporters", h.CreateExporter)
		r.Get("/exporters", h.ListExporters)
		r.Get("/exporters/{id}", h.GetExporter)
		r.Post("/stream", h.StreamEvent)
		r.Get("/events", h.ListEvents)
	})
	return r
}

func (h *Handler) CreateExporter(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	var req domain.CreateExporterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, 400, "invalid body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, 400, err.Error())
		return
	}
	exp := &domain.SIEMExporter{
		LegalEntityID: req.LegalEntityID,
		Name:          req.Name,
		Platform:      req.Platform,
		EndpointURL:   req.EndpointURL,
		AuthToken:     req.AuthToken,
	}
	if err := h.store.CreateExporter(r.Context(), tenantID, exp); err != nil {
		h.errJSON(w, 500, "failed to create exporter")
		return
	}
	h.okJSON(w, 201, exp)
}

func (h *Handler) GetExporter(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	exp, err := h.store.GetExporterByID(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.errJSON(w, 404, "exporter not found")
		return
	}
	h.okJSON(w, 200, exp)
}

func (h *Handler) ListExporters(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	exps, _ := h.store.ListExporters(r.Context(), tenantID, r.URL.Query().Get("legal_entity_id"))
	if exps == nil {
		exps = []domain.SIEMExporter{}
	}
	h.okJSON(w, 200, map[string]interface{}{"data": exps, "count": len(exps)})
}

func (h *Handler) StreamEvent(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	var req domain.StreamEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, 400, "invalid body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, 400, err.Error())
		return
	}
	evt := &domain.SIEMEvent{
		ExporterID: req.ExporterID,
		SourceSvc:  req.SourceSvc,
		EventType:  req.EventType,
		Severity:   req.Severity,
		Message:    req.Message,
		Payload:    req.Payload,
	}
	if err := h.store.StreamEvent(r.Context(), tenantID, evt); err != nil {
		h.errJSON(w, 404, err.Error())
		return
	}
	h.okJSON(w, 201, evt)
}

func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request) {
	tenantID := getTenant(r)
	evts, _ := h.store.ListEvents(r.Context(), tenantID, r.URL.Query().Get("exporter_id"))
	if evts == nil {
		evts = []domain.SIEMEvent{}
	}
	h.okJSON(w, 200, map[string]interface{}{"data": evts, "count": len(evts)})
}

func (h *Handler) okJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func (h *Handler) errJSON(w http.ResponseWriter, code int, msg string) {
	h.okJSON(w, code, map[string]string{"error": msg})
}
