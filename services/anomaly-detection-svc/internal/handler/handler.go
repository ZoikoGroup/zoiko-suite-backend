package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/anomaly-detection-svc/internal/authz"
	"zoiko.io/anomaly-detection-svc/internal/domain"
	"zoiko.io/anomaly-detection-svc/internal/events"
	"zoiko.io/anomaly-detection-svc/internal/middleware"
	"zoiko.io/anomaly-detection-svc/internal/store"
)

const (
	ANOMALY_DETECT = "ANOMALY_DETECT"
	ANOMALY_ACK    = "ANOMALY_ACK"
	RULE_CREATE    = "RULE_CREATE"

	// AI-04 governed advisory layer actions.
	ANOMALY_MODEL_MANAGE  = "ANOMALY_MODEL_MANAGE"
	ANOMALY_GOVERNED_READ = "ANOMALY_GOVERNED_READ"
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

	// AI-04 governed advisory layer (ZS-SVC-N-001 §4/§13 Wave 8) — additive,
	// lives alongside the rule-engine surface above under its own path.
	r.Route("/v1/anomaly-governance", func(r chi.Router) {
		r.Post("/models", h.RegisterAnomalyModel)
		r.Post("/runs", h.RunDetection)
		r.Get("/runs/{runID}/signals", h.GetSignalsByRun)
		r.Get("/signals/{signalID}", h.GetGovernedSignal)
		r.Post("/signals/{signalID}:acknowledge", h.AcknowledgeSignal)
		r.Post("/signals/{signalID}:escalate", h.EscalateForReview)
		r.Post("/signals/{signalID}:close", h.CloseSignal)
		r.Get("/signals/{signalID}/disposition", h.GetDisposition)
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

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, ANOMALY_DETECT); err != nil {
		h.writeAuthzErr(w, err)
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

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "anomaly.detected", SubjectID: rec.AnomalyID, TenantID: tenantID,
		LegalEntityID: rec.LegalEntityID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: rec,
	})
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

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAnomalyRecordNotFound) {
			writeError(w, http.StatusNotFound, "anomaly record not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get anomaly record")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, ANOMALY_ACK); err != nil {
		h.writeAuthzErr(w, err)
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
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: eventType, SubjectID: id, TenantID: tenantID,
		LegalEntityID: rec.LegalEntityID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: rec,
	})
	writeJSON(w, http.StatusOK, rec)
}

func (h *Handler) CreateRule(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RuleName == "" || req.DomainName == "" || req.MetricType == "" {
		writeError(w, http.StatusBadRequest, "rule_name, domain_name, and metric_type are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// Detection rules are tenant-scoped, not tied to a single legal entity;
	// tenantID is the closest available scoping identifier for the check.
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, RULE_CREATE); err != nil {
		h.writeAuthzErr(w, err)
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

// requirePrincipal reads the caller's identity from X-Principal-Id, set by
// the gateway after identity verification. A request with no resolved
// principal never passed identity verification — fail closed with 401.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// writeAuthzErr maps an authz.CheckAllowed error to the appropriate HTTP
// response. Denial is 403; any other error (including authorization-svc
// being unreachable) is 503 — fail closed, never allow silently.
func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "authorization denied")
		return
	}
	h.logger.Error("authorization check failed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

// ─────────────────────────────────────────────────────────────────────────────
// AI-04 governed advisory layer (ZS-SVC-N-001 §4/§13 Wave 8) — additive.
// ─────────────────────────────────────────────────────────────────────────────

func idempotencyClaim(principalID, operation, resourceID, requestSHA256, key string) domain.IdempotencyClaim {
	return domain.IdempotencyClaim{
		OwnerScope: domain.SellerScope, PrincipalID: principalID, Key: key,
		Operation: operation, RequestSHA256: requestSHA256, ResourceID: resourceID,
	}
}

func readBodyHashed(r *http.Request, v interface{}) (string, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, v); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (h *Handler) RegisterAnomalyModel(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, ANOMALY_MODEL_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	var req domain.RegisterAnomalyModelRequest
	if _, err := readBodyHashed(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	got, err := h.store.RegisterAnomalyModel(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.respondGovernedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *Handler) RunDetection(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, ANOMALY_DETECT); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	var req domain.RunDetectionRequest
	reqHash, err := readBodyHashed(r, &req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := idempotencyClaim(principalID, "RunDetection", req.DomainName+"|"+req.ModelVersion, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RunDetection(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondGovernedError(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "AI.AnomalyDetected", SubjectID: got.RunID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: got,
	})
	writeJSON(w, http.StatusCreated, got)
}

func (h *Handler) GetSignalsByRun(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, ANOMALY_GOVERNED_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	items, err := h.store.GetSignalsByRun(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondGovernedError(w, err)
		return
	}
	if items == nil {
		items = []domain.GovernedAnomalySignal{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetGovernedSignal(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, ANOMALY_GOVERNED_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	got, err := h.store.GetGovernedSignal(r.Context(), tenantID, chi.URLParam(r, "signalID"))
	if err != nil {
		h.respondGovernedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *Handler) AcknowledgeSignal(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, ANOMALY_ACK); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	signalID := chi.URLParam(r, "signalID")
	claim := idempotencyClaim(principalID, "AcknowledgeSignal", signalID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.AcknowledgeSignal(r.Context(), tenantID, signalID, principalID, claim)
	if err != nil {
		h.respondGovernedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *Handler) EscalateForReview(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, ANOMALY_ACK); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	signalID := chi.URLParam(r, "signalID")
	var req domain.EscalateForReviewRequest
	reqHash, err := readBodyHashed(r, &req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := idempotencyClaim(principalID, "EscalateForReview", signalID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.EscalateForReview(r.Context(), tenantID, signalID, req, principalID, claim)
	if err != nil {
		h.respondGovernedError(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "AI.AnomalyDispositioned", SubjectID: signalID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: got,
	})
	writeJSON(w, http.StatusOK, got)
}

func (h *Handler) CloseSignal(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, ANOMALY_ACK); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	signalID := chi.URLParam(r, "signalID")
	var req domain.CloseSignalRequest
	reqHash, err := readBodyHashed(r, &req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := idempotencyClaim(principalID, "CloseSignal", signalID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.CloseSignal(r.Context(), tenantID, signalID, req, principalID, claim)
	if err != nil {
		h.respondGovernedError(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "AI.AnomalyDispositioned", SubjectID: signalID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: got,
	})
	writeJSON(w, http.StatusOK, got)
}

func (h *Handler) GetDisposition(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, ANOMALY_GOVERNED_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	got, err := h.store.GetDisposition(r.Context(), tenantID, chi.URLParam(r, "signalID"))
	if err != nil {
		h.respondGovernedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *Handler) respondGovernedError(w http.ResponseWriter, err error) {
	var replay *domain.IdempotentReplayError
	if errors.As(err, &replay) {
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, map[string]string{"resource_id": replay.ResourceID})
		return
	}
	switch {
	case errors.Is(err, domain.ErrSignalNotFound), errors.Is(err, domain.ErrAnomalyModelNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrSignalNotDetected), errors.Is(err, domain.ErrSignalNotReviewable),
		errors.Is(err, domain.ErrDriftExceedsThreshold), errors.Is(err, domain.ErrIdempotencyKeyReused):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("anomaly-detection-svc governance request failed", zap.Error(err))
		writeError(w, http.StatusBadRequest, err.Error())
	}
}
