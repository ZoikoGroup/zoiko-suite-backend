package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/anomaly-detection-svc/internal/authz"
	"zoiko.io/anomaly-detection-svc/internal/domain"
	"zoiko.io/anomaly-detection-svc/internal/events"
	"zoiko.io/anomaly-detection-svc/internal/middleware"
	"zoiko.io/anomaly-detection-svc/internal/store"
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
	r.Route("/v1/anomalies", func(r chi.Router) {
		r.Post("/detect", h.Detect)
		r.Get("/", h.ListAnomalies)
		r.Get("/{id}", h.GetByID)
		r.Post("/{id}/status", h.UpdateStatus)

		r.Post("/rules", h.CreateRule)
		r.Get("/rules", h.ListRules)
	})
}

func (h *Handler) Detect(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.DetectAnomalyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.DomainName == "" || req.SourceEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, domain_name, and source_entity_id are required")
		return
	}

	score, severity := domain.CalculateAnomalyScore(req.ObservedValue, req.ExpectedValue, req.StdDeviation)
	desc := req.Description
	if desc == "" {
		desc = fmt.Sprintf("Anomaly detected in %s metric '%s': observed %.2f vs expected %.2f (score: %.2f)",
			req.DomainName, req.MetricType, req.ObservedValue, req.ExpectedValue, score)
	}

	rec := &domain.AnomalyRecord{
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		DomainName:     req.DomainName,
		SourceEntityID: req.SourceEntityID,
		RuleID:         req.RuleID,
		Severity:       severity,
		AnomalyScore:   score,
		ObservedValue:  req.ObservedValue,
		ExpectedValue:  req.ExpectedValue,
		Description:    desc,
		Status:         domain.StatusOpen,
	}

	if err := h.store.Detect(r.Context(), rec); err != nil {
		h.logger.Error("detect anomaly failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to record anomaly")
		return
	}

	_ = h.publisher.Publish(r.Context(), "anomaly.detected", rec.AnomalyID, tenantID, rec)
	writeJSON(w, http.StatusCreated, rec)
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rec, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAnomalyRecordNotFound) {
			writeError(w, http.StatusNotFound, "anomaly record not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get anomaly record")
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *Handler) ListAnomalies(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	domainName := r.URL.Query().Get("domain_name")
	severity := r.URL.Query().Get("severity")
	status := r.URL.Query().Get("status")

	records, err := h.store.ListAnomalies(r.Context(), legalEntityID, domainName, severity, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list anomaly records")
		return
	}
	if records == nil {
		records = []domain.AnomalyRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"anomalies": records,
		"total":     len(records),
	})
}

func (h *Handler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.UpdateStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Status == "" || req.InvestigatedBy == "" {
		writeError(w, http.StatusBadRequest, "status and investigated_by are required")
		return
	}

	rec, err := h.store.UpdateStatus(r.Context(), id, &req)
	if err != nil {
		if errors.Is(err, domain.ErrAnomalyRecordNotFound) {
			writeError(w, http.StatusNotFound, "anomaly record not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update anomaly status")
		return
	}

	eventType := "anomaly.investigated"
	if rec.Status == domain.StatusResolved {
		eventType = "anomaly.resolved"
	}
	_ = h.publisher.Publish(r.Context(), eventType, id, tenantID, rec)
	writeJSON(w, http.StatusOK, rec)
}

func (h *Handler) CreateRule(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RuleName == "" || req.DomainName == "" || req.MetricType == "" {
		writeError(w, http.StatusBadRequest, "rule_name, domain_name, and metric_type are required")
		return
	}

	cutoff := req.ZScoreCutoff
	if cutoff <= 0 {
		cutoff = 3.00
	}

	rule := &domain.AnomalyRule{
		RuleName:       req.RuleName,
		DomainName:     req.DomainName,
		MetricType:     req.MetricType,
		ThresholdValue: req.ThresholdValue,
		ZScoreCutoff:   cutoff,
	}

	if err := h.store.CreateRule(r.Context(), rule); err != nil {
		h.logger.Error("create anomaly rule failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create anomaly rule")
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (h *Handler) ListRules(w http.ResponseWriter, r *http.Request) {
	domainName := r.URL.Query().Get("domain_name")

	rules, err := h.store.ListRules(r.Context(), domainName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list anomaly rules")
		return
	}
	if rules == nil {
		rules = []domain.AnomalyRule{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"rules": rules,
		"total": len(rules),
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
