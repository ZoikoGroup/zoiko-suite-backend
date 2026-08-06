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

	"zoiko.io/spend-controls-svc/internal/domain"
	svcmiddleware "zoiko.io/spend-controls-svc/internal/middleware"
)

type Store interface {
	CreatePolicy(ctx context.Context, p *domain.SpendPolicy) error
	ListPolicies(ctx context.Context, legalEntityID, category string) ([]domain.SpendPolicy, error)
	FindActivePolicy(ctx context.Context, legalEntityID, category string) (*domain.SpendPolicy, error)
	SumConsumption(ctx context.Context, spendPolicyID string, since time.Time) (float64, error)
	FindConsumptionByCorrelation(ctx context.Context, correlationID string) (*domain.SpendConsumption, error)
	RecordConsumption(ctx context.Context, c *domain.SpendConsumption) (created bool, err error)
	ListConsumptions(ctx context.Context, legalEntityID, spendPolicyID string) ([]domain.SpendConsumption, error)
}

type Publisher interface {
	PublishThresholdBreached(ctx context.Context, correlationID string, check domain.SpendCheckRequest, policy domain.SpendPolicy, projected float64)
	PublishBlockApplied(ctx context.Context, correlationID string, check domain.SpendCheckRequest, policy domain.SpendPolicy)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

const (
	actionPolicyManage = "SPEND_POLICY_MANAGE"
	actionPolicyView   = "SPEND_POLICY_VIEW"
	actionCheckSubmit  = "SPEND_CHECK_SUBMIT"
)

var validPeriods = map[string]bool{"PER_TRANSACTION": true, "MONTHLY": true, "ANNUAL": true}

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/spend-policies", func(r chi.Router) {
		r.Post("/", h.CreatePolicy)
		r.Get("/", h.ListPolicies)
	})
	r.Route("/v1/spend-checks", func(r chi.Router) {
		r.Post("/", h.SubmitCheck)
	})
	r.Route("/v1/spend-consumptions", func(r chi.Router) {
		r.Get("/", h.ListConsumptions)
	})
}

// ── POST /v1/spend-policies ───────────────────────────────────────────────────

func (h *Handler) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	var req domain.CreatePolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.Category == "" || req.CurrencyCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, category, currency_code are required")
		return
	}
	if req.ThresholdAmount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_threshold", "threshold_amount must be > 0")
		return
	}
	if !validPeriods[req.Period] {
		writeError(w, http.StatusBadRequest, "invalid_period", "period must be PER_TRANSACTION, MONTHLY, or ANNUAL")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionPolicyManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	now := time.Now().UTC()
	policy := &domain.SpendPolicy{
		SpendPolicyID:        uuid.NewString(),
		TenantID:             svcmiddleware.TenantFromContext(r.Context()),
		LegalEntityID:        req.LegalEntityID,
		Category:             req.Category,
		Period:               req.Period,
		ThresholdAmount:      req.ThresholdAmount,
		CurrencyCode:         req.CurrencyCode,
		ActiveFlag:           true,
		CreatedByPrincipalID: principalID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	if err := h.store.CreatePolicy(r.Context(), policy); err != nil {
		h.log.Error("failed to create spend policy", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, policy)
}

// ── GET /v1/spend-policies ────────────────────────────────────────────────────

func (h *Handler) ListPolicies(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	category := r.URL.Query().Get("category")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionPolicyView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListPolicies(r.Context(), legalEntityID, category)
	if err != nil {
		h.log.Error("failed to list spend policies", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.SpendPolicy{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/spend-consumptions ────────────────────────────────────────────────

func (h *Handler) ListConsumptions(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	spendPolicyID := r.URL.Query().Get("spend_policy_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionPolicyView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListConsumptions(r.Context(), legalEntityID, spendPolicyID)
	if err != nil {
		h.log.Error("failed to list spend consumptions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.SpendConsumption{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/spend-checks ─────────────────────────────────────────────────────

// SubmitCheck evaluates whether a proposed spend would breach the caller's
// configured policy for this legal entity + category, and — only when
// allowed — records the consumption so later checks see it.
//
// Idempotent on (tenant_id, correlation_id): a retried check replays the
// stored decision rather than re-evaluating consumption, which would
// double-count the spend on a network retry. A BLOCKED decision is
// deliberately never recorded as consumption, so a rejected attempt cannot
// itself eat into the budget it was blocked from spending.
func (h *Handler) SubmitCheck(w http.ResponseWriter, r *http.Request) {
	var req domain.SpendCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.Category == "" || req.CurrencyCode == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, category, currency_code, correlation_id are required")
		return
	}
	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_amount", "amount must be > 0")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCheckSubmit); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	existing, err := h.store.FindConsumptionByCorrelation(r.Context(), req.CorrelationID)
	if err != nil {
		h.log.Error("failed to look up existing consumption", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusOK, domain.SpendCheckResponse{
			DecisionOutcome: existing.DecisionOutcome,
			DecisionBasis:   "replayed_prior_decision",
			SpendPolicyID:   existing.SpendPolicyID,
			ConsumptionID:   existing.ConsumptionID,
		})
		return
	}

	policy, err := h.store.FindActivePolicy(r.Context(), req.LegalEntityID, req.Category)
	if err != nil {
		h.log.Error("failed to look up spend policy", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if policy == nil {
		writeJSON(w, http.StatusOK, domain.SpendCheckResponse{
			DecisionOutcome: "ALLOWED",
			DecisionBasis:   "no_policy_configured",
		})
		return
	}

	var priorConsumption float64
	if policy.Period != "PER_TRANSACTION" {
		priorConsumption, err = h.store.SumConsumption(r.Context(), policy.SpendPolicyID, periodStart(policy.Period))
		if err != nil {
			h.log.Error("failed to sum spend consumption", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
			return
		}
	}
	projected := priorConsumption + req.Amount

	correlationID := getCorrelationID(r)

	if projected > policy.ThresholdAmount {
		h.publisher.PublishThresholdBreached(r.Context(), correlationID, req, *policy, projected)
		h.publisher.PublishBlockApplied(r.Context(), correlationID, req, *policy)
		writeJSON(w, http.StatusOK, domain.SpendCheckResponse{
			DecisionOutcome:  "BLOCKED",
			DecisionBasis:    "threshold_exceeded",
			SpendPolicyID:    policy.SpendPolicyID,
			PriorConsumption: priorConsumption,
			ThresholdAmount:  policy.ThresholdAmount,
		})
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	consumption := &domain.SpendConsumption{
		ConsumptionID:         uuid.NewString(),
		TenantID:              tenantID,
		LegalEntityID:         req.LegalEntityID,
		SpendPolicyID:         policy.SpendPolicyID,
		Amount:                req.Amount,
		CurrencyCode:          req.CurrencyCode,
		SourceReference:       req.SourceReference,
		CorrelationID:         req.CorrelationID,
		DecisionOutcome:       "ALLOWED",
		RecordedByPrincipalID: principalID,
		RecordedAt:            time.Now().UTC(),
	}

	if _, err := h.store.RecordConsumption(r.Context(), consumption); err != nil {
		h.log.Error("failed to record spend consumption", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, domain.SpendCheckResponse{
		DecisionOutcome:  "ALLOWED",
		DecisionBasis:    "within_threshold",
		SpendPolicyID:    policy.SpendPolicyID,
		PriorConsumption: priorConsumption,
		ThresholdAmount:  policy.ThresholdAmount,
		ConsumptionID:    consumption.ConsumptionID,
	})
}

// ── Helpers ────────────────────────────────────────────────────────────────

// periodStart returns the start of the current enforcement window for a
// policy's period — the point consumption is summed from.
func periodStart(period string) time.Time {
	now := time.Now().UTC()
	if period == "ANNUAL" {
		return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC) // MONTHLY
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func getCorrelationID(r *http.Request) string {
	cid := r.Header.Get("X-Correlation-ID")
	if cid == "" {
		return uuid.NewString()
	}
	return cid
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code":    code,
		"error_message": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
