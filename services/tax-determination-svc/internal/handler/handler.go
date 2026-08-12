package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/tax-determination-svc/internal/authz"
	"zoiko.io/tax-determination-svc/internal/domain"
	"zoiko.io/tax-determination-svc/internal/events"
	"zoiko.io/tax-determination-svc/internal/middleware"
	"zoiko.io/tax-determination-svc/internal/rules"
	"zoiko.io/tax-determination-svc/internal/store"
)

// snapshotTaxRule computes a content-addressed reference over the actual
// rule fields tax-rules-svc returned, pinning a determination to the exact
// rule content applied — independent of later edits to the mutable rule row
// rule_id points at. Returns nil for the zero-tax fallback (tax-rules-svc
// unreachable): there is no real rule content to snapshot, and fabricating
// one would misrepresent a fallback as a governed rule application.
func snapshotTaxRule(rule *rules.TaxRuleDTO) *string {
	// "trule-fallback" is this handler's own (currently unreachable) local
	// fallback; "trule-default-fallback"/"trule-default-zero" are
	// rules.Client's actual fallback sentinels — a transport error or an
	// empty rule set from tax-rules-svc, respectively. None represent a
	// real, governed rule application.
	if rule == nil || rule.RuleID == "trule-fallback" ||
		rule.RuleID == "trule-default-fallback" || rule.RuleID == "trule-default-zero" {
		return nil
	}
	canonical := fmt.Sprintf("%s|%s|%s|%.6f|%.6f|%s",
		rule.RuleID, rule.RuleCode, rule.Category, rule.TaxRatePercentage, rule.StandardDeductions, rule.Status)
	sum := sha256.Sum256([]byte(canonical))
	hash := hex.EncodeToString(sum[:])
	return &hash
}

// Action constants for authorization-svc calls, shaped <RESOURCE>_<VERB>.
const (
	ActionTaxDeterminationCreate   = "TAX_DETERMINATION_CREATE"
	ActionTaxDeterminationOverride = "TAX_DETERMINATION_OVERRIDE"
)

// AuthzChecker is the subset of authz.Client's contract the handler depends
// on. It is an interface (rather than *authz.Client directly) so tests can
// substitute a stub without exercising real HTTP calls.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

var _ AuthzChecker = (*authz.Client)(nil)

type Handler struct {
	store       store.Store
	publisher   events.Publisher
	authz       AuthzChecker
	rulesClient *rules.Client
	logger      *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, rc *rules.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, rulesClient: rc, logger: logger}
}

// authorize extracts the caller's principal from the X-Principal-Id header
// and checks the action against authorization-svc before any mutation is
// performed. It writes the appropriate error response itself when the
// caller should not proceed.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, legalEntityID, actionType string) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		if errors.Is(err, authz.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		}
		return "", false
	}
	return principalID, true
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/tax-determinations", func(r chi.Router) {
		r.Post("/", h.DetermineTax)
		r.Get("/", h.ListDeterminations)
		r.Get("/{id}", h.GetDetermination)
		r.Post("/{id}/override", h.OverrideDetermination)
	})
}

func (h *Handler) DetermineTax(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.DetermineTaxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TransactionID == "" || req.JurisdictionID == "" || req.TaxCategory == "" || req.GrossAmount <= 0 {
		writeError(w, http.StatusBadRequest, "transaction_id, jurisdiction_id, tax_category, and positive gross_amount are required")
		return
	}

	if _, ok := h.authorize(w, r, req.LegalEntityID, ActionTaxDeterminationCreate); !ok {
		return
	}

	// Fetch dynamic tax rule from tax-rules-svc
	rule, err := h.rulesClient.FetchActiveRule(r.Context(), tenantID, req.JurisdictionID, req.TaxCategory)
	if err != nil {
		h.logger.Warn("failed to fetch tax rule, using zero tax fallback", zap.Error(err))
		rule = &rules.TaxRuleDTO{
			RuleID:            "trule-fallback",
			TaxRatePercentage: 0,
		}
	}

	taxableAmount := req.GrossAmount - req.ExemptAmount
	if taxableAmount < 0 {
		taxableAmount = 0
	}
	calculatedTax := taxableAmount * (rule.TaxRatePercentage / 100.0)

	det := &domain.TaxDetermination{
		TenantID:            tenantID,
		TransactionID:       req.TransactionID,
		SourceModule:        req.SourceModule,
		LegalEntityID:       req.LegalEntityID,
		JurisdictionID:      req.JurisdictionID,
		RuleID:              rule.RuleID,
		TaxLogicSnapshotID:  snapshotTaxRule(rule),
		TaxCategory:         req.TaxCategory,
		GrossAmount:         req.GrossAmount,
		TaxableAmount:       taxableAmount,
		TaxRatePercentage:   rule.TaxRatePercentage,
		CalculatedTaxAmount: calculatedTax,
		ExemptAmount:        req.ExemptAmount,
		Currency:            req.Currency,
		Status:              domain.StatusCalculated,
		EffectiveFrom:       req.EffectiveFrom,
		EvaluatedBy:         req.EvaluatedBy,
	}

	if err := h.store.CreateDetermination(r.Context(), det); err != nil {
		h.logger.Error("create tax determination failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to record tax determination")
		return
	}

	_ = h.publisher.Publish(r.Context(), "tax_determination.calculated", det.DeterminationID, tenantID, det)
	writeJSON(w, http.StatusCreated, det)
}

func (h *Handler) GetDetermination(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	det, err := h.store.GetDetermination(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTaxDeterminationNotFound) {
			writeError(w, http.StatusNotFound, "tax determination not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get tax determination")
		return
	}
	writeJSON(w, http.StatusOK, det)
}

func (h *Handler) ListDeterminations(w http.ResponseWriter, r *http.Request) {
	transactionID := r.URL.Query().Get("transaction_id")
	jurisdictionID := r.URL.Query().Get("jurisdiction_id")
	status := r.URL.Query().Get("status")
	determinations, err := h.store.ListDeterminations(r.Context(), transactionID, jurisdictionID, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tax determinations")
		return
	}
	if determinations == nil {
		determinations = []domain.TaxDetermination{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"determinations": determinations, "total": len(determinations)})
}

func (h *Handler) OverrideDetermination(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.OverrideTaxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required for tax override")
		return
	}

	existing, err := h.store.GetDetermination(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTaxDeterminationNotFound) {
			writeError(w, http.StatusNotFound, "tax determination not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get tax determination")
		return
	}

	if _, ok := h.authorize(w, r, existing.LegalEntityID, ActionTaxDeterminationOverride); !ok {
		return
	}

	det, err := h.store.OverrideDetermination(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrTaxDeterminationNotFound):
			writeError(w, http.StatusNotFound, "tax determination not found")
		case errors.Is(err, domain.ErrAlreadyOverridden):
			writeError(w, http.StatusConflict, "tax determination is already overridden")
		default:
			writeError(w, http.StatusInternalServerError, "failed to override tax determination")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), "tax_determination.overridden", id, tenantID, det)
	writeJSON(w, http.StatusOK, det)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
