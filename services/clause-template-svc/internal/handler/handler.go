package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/clause-template-svc/internal/authz"
	"zoiko.io/clause-template-svc/internal/domain"
	"zoiko.io/clause-template-svc/internal/events"
	"zoiko.io/clause-template-svc/internal/middleware"
	"zoiko.io/clause-template-svc/internal/store"
)

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/clauses", func(r chi.Router) {
		r.Post("/", h.CreateClause)
		r.Get("/", h.ListClauses)
		r.Get("/{id}", h.GetClause)
		r.Put("/{id}", h.UpdateClause)
	})

	r.Route("/v1/templates", func(r chi.Router) {
		r.Post("/", h.CreateTemplate)
		r.Get("/", h.ListTemplates)
		r.Get("/{id}", h.GetTemplate)
		r.Put("/{id}", h.UpdateTemplate)
	})

	r.Route("/v1/clause-templates", func(r chi.Router) {
		r.Post("/", h.CreateTemplate)
		r.Get("/", h.ListTemplates)
		r.Get("/{id}", h.GetTemplate)
		r.Put("/{id}", h.UpdateTemplate)
	})
}

// --- Clause Handlers ---

func (h *Handler) CreateClause(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateClauseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.Body == "" || req.Category == "" {
		writeError(w, http.StatusBadRequest, "title, body, and category are required")
		return
	}

	c := &domain.Clause{
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		Title:          req.Title,
		Category:       req.Category,
		Body:           req.Body,
		JurisdictionID: req.JurisdictionID,
		EffectiveFrom:  req.EffectiveFrom,
		EffectiveTo:    req.EffectiveTo,
		CreatedBy:      req.CreatedBy,
	}

	if err := h.store.CreateClause(r.Context(), c); err != nil {
		h.logger.Error("create clause failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create clause")
		return
	}

	_ = h.publisher.Publish(r.Context(), "clause.created", c.ClauseID, tenantID, c)
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) GetClause(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	c, err := h.store.GetClause(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrClauseNotFound) {
			writeError(w, http.StatusNotFound, "clause not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get clause")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ListClauses(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	category := r.URL.Query().Get("category")
	clauses, err := h.store.ListClauses(r.Context(), legalEntityID, category)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list clauses")
		return
	}
	if clauses == nil {
		clauses = []domain.Clause{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"clauses": clauses, "total": len(clauses)})
}

func (h *Handler) UpdateClause(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetClause(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrClauseNotFound) {
			writeError(w, http.StatusNotFound, "clause not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch clause")
		return
	}

	var req domain.UpdateClauseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Title != "" {
		existing.Title = req.Title
	}
	if req.Category != "" {
		existing.Category = req.Category
	}
	if req.Body != "" {
		existing.Body = req.Body
	}
	if req.JurisdictionID != "" {
		existing.JurisdictionID = req.JurisdictionID
	}
	if req.EffectiveTo != nil {
		existing.EffectiveTo = req.EffectiveTo
	}

	if err := h.store.UpdateClause(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update clause")
		return
	}

	_ = h.publisher.Publish(r.Context(), "clause.updated", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

// --- Template Handlers ---

func (h *Handler) CreateTemplate(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.ContractType == "" {
		writeError(w, http.StatusBadRequest, "title and contract_type are required")
		return
	}

	t := &domain.ContractTemplate{
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		Title:          req.Title,
		ContractType:   req.ContractType,
		Description:    req.Description,
		ClauseIDs:      req.ClauseIDs,
		JurisdictionID: req.JurisdictionID,
		EffectiveFrom:  req.EffectiveFrom,
		EffectiveTo:    req.EffectiveTo,
		CreatedBy:      req.CreatedBy,
	}

	if err := h.store.CreateTemplate(r.Context(), t); err != nil {
		h.logger.Error("create template failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create template")
		return
	}

	_ = h.publisher.Publish(r.Context(), "template.created", t.TemplateID, tenantID, t)
	writeJSON(w, http.StatusCreated, t)
}

func (h *Handler) GetTemplate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t, err := h.store.GetTemplate(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTemplateNotFound) {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get template")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *Handler) ListTemplates(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	contractType := r.URL.Query().Get("contract_type")
	templates, err := h.store.ListTemplates(r.Context(), legalEntityID, contractType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list templates")
		return
	}
	if templates == nil {
		templates = []domain.ContractTemplate{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"templates": templates, "total": len(templates)})
}

func (h *Handler) UpdateTemplate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetTemplate(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTemplateNotFound) {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch template")
		return
	}

	var req domain.UpdateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Title != "" {
		existing.Title = req.Title
	}
	if req.ContractType != "" {
		existing.ContractType = req.ContractType
	}
	if req.Description != "" {
		existing.Description = req.Description
	}
	if req.ClauseIDs != nil {
		existing.ClauseIDs = req.ClauseIDs
	}
	if req.JurisdictionID != "" {
		existing.JurisdictionID = req.JurisdictionID
	}
	if req.EffectiveTo != nil {
		existing.EffectiveTo = req.EffectiveTo
	}

	if err := h.store.UpdateTemplate(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update template")
		return
	}

	_ = h.publisher.Publish(r.Context(), "template.updated", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
