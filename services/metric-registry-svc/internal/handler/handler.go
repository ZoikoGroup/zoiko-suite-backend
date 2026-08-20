package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/metric-registry-svc/internal/authz"
	"zoiko.io/metric-registry-svc/internal/domain"
	"zoiko.io/metric-registry-svc/internal/events"
	"zoiko.io/metric-registry-svc/internal/store"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

const (
	ReportMetricCreate  = "REPORT_METRIC_CREATE"
	ReportMetricPublish = "REPORT_METRIC_PUBLISH"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthzChecker
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, platformScopeID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		}
		return false
	}
	return true
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/report-metrics", func(r chi.Router) {
		r.Post("/", h.CreateMetricDefinition)
		r.Get("/{metric_code}", h.GetActiveMetricDefinition)
		r.Get("/{metric_code}/versions", h.ListMetricVersions)
		r.Post("/{metric_code}/versions", h.PublishNewVersion)
	})
}

func missingCreateField(req domain.CreateReportMetricRequest) string {
	switch {
	case req.MetricCode == "":
		return "metric_code"
	case req.MetricName == "":
		return "metric_name"
	case req.FormulaDescription == "":
		return "formula_description"
	case req.OwnerPrincipalID == "":
		return "owner_principal_id"
	case req.EffectiveFrom == "":
		return "effective_from"
	default:
		return ""
	}
}

// CreateMetricDefinition handles POST /v1/report-metrics — always creates
// version 1 of a new metric_code.
func (h *Handler) CreateMetricDefinition(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateReportMetricRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if missing := missingCreateField(req); missing != "" {
		writeError(w, http.StatusBadRequest, missing+" is required")
		return
	}
	effectiveFrom, err := time.Parse(time.RFC3339, req.EffectiveFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "effective_from must be RFC3339")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, ReportMetricCreate) {
		return
	}

	d := &domain.ReportMetricDefinition{
		MetricDefinitionID:     uuid.NewString(),
		MetricCode:             req.MetricCode,
		MetricName:             req.MetricName,
		FormulaDescription:     req.FormulaDescription,
		DataSources:            req.DataSources,
		OwnerPrincipalID:       req.OwnerPrincipalID,
		IntelligenceDisclaimer: domain.DefaultIntelligenceDisclaimer,
		Version:                1,
		DefinitionStatus:       "ACTIVE",
		EffectiveFrom:          effectiveFrom,
		CreatedAt:              time.Now().UTC(),
		CreatedByPrincipalID:   principalID,
	}
	if err := h.store.CreateMetricDefinition(r.Context(), d); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "metric_code already exists")
			return
		}
		h.logger.Error("create metric definition failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create metric definition")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "report_metric.created", EntityID: d.MetricDefinitionID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: d,
	})
	writeJSON(w, http.StatusCreated, d)
}

// GetActiveMetricDefinition handles GET /v1/report-metrics/{metric_code}.
func (h *Handler) GetActiveMetricDefinition(w http.ResponseWriter, r *http.Request) {
	d, err := h.store.GetActiveMetricDefinition(r.Context(), chi.URLParam(r, "metric_code"))
	if err != nil {
		if errors.Is(err, domain.ErrMetricNotFound) {
			writeError(w, http.StatusNotFound, "report metric not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get metric definition")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ListMetricVersions handles GET /v1/report-metrics/{metric_code}/versions.
func (h *Handler) ListMetricVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := h.store.ListMetricVersions(r.Context(), chi.URLParam(r, "metric_code"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list metric versions")
		return
	}
	if versions == nil {
		versions = []domain.ReportMetricDefinition{}
	}
	writeJSON(w, http.StatusOK, versions)
}

// PublishNewVersion handles POST /v1/report-metrics/{metric_code}/versions
// — publishes a new version, atomically superseding whatever was ACTIVE.
func (h *Handler) PublishNewVersion(w http.ResponseWriter, r *http.Request) {
	metricCode := chi.URLParam(r, "metric_code")
	var req domain.PublishMetricVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.MetricName == "" || req.FormulaDescription == "" || req.OwnerPrincipalID == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "metric_name, formula_description, owner_principal_id, and effective_from are required")
		return
	}
	effectiveFrom, err := time.Parse(time.RFC3339, req.EffectiveFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "effective_from must be RFC3339")
		return
	}

	current, err := h.store.GetActiveMetricDefinition(r.Context(), metricCode)
	if err != nil {
		if errors.Is(err, domain.ErrMetricNotFound) {
			writeError(w, http.StatusNotFound, "report metric not found — create version 1 first via POST /v1/report-metrics")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch current metric definition")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, ReportMetricPublish) {
		return
	}

	next := &domain.ReportMetricDefinition{
		MetricDefinitionID:     uuid.NewString(),
		MetricCode:             metricCode,
		MetricName:             req.MetricName,
		FormulaDescription:     req.FormulaDescription,
		DataSources:            req.DataSources,
		OwnerPrincipalID:       req.OwnerPrincipalID,
		IntelligenceDisclaimer: domain.DefaultIntelligenceDisclaimer,
		Version:                current.Version + 1,
		DefinitionStatus:       "ACTIVE",
		EffectiveFrom:          effectiveFrom,
		CreatedAt:              time.Now().UTC(),
		CreatedByPrincipalID:   principalID,
	}
	if err := h.store.PublishNewVersion(r.Context(), next); err != nil {
		h.logger.Error("publish new metric version failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to publish new metric version")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "report_metric.version_published", EntityID: next.MetricDefinitionID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: next,
	})
	writeJSON(w, http.StatusCreated, next)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
