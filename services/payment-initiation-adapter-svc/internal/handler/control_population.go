package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payment-initiation-adapter-svc/internal/authz"
	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	svcmiddleware "zoiko.io/payment-initiation-adapter-svc/internal/middleware"
)

// ZS-CONTROL-001 §9 control population endpoint — see
// docs/architecture/control-population-contract.md ("payment-initiation-adapter-svc").

const (
	// PaymentInitiationControlPopulationRead is the action the verified human caller needs, for the entity, to read
	// this population.
	PaymentInitiationControlPopulationRead = "PAYMENT_INITIATION_CONTROL_POPULATION_READ"

	defaultPopulationLimit = 1000
	maxPopulationLimit     = 5000
)

var paymentAttemptsParams = map[string]bool{"legal_entity_id": true, "period_id": true, "limit": true, "cursor": true}

type controlPopulationResponse struct {
	Records        []domain.ControlPopulationRecord `json:"records"`
	NextCursor     string                           `json:"next_cursor"`
	Watermark      string                           `json:"watermark"`
	DeclaredTotals domain.ControlDeclaredTotals     `json:"declared_totals"`
}

// GetControlPopulation serves GET /v1/control-populations/{population}; only payment-attempts exists.
func (h *Handler) GetControlPopulation(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "population") != domain.PopulationPaymentAttempts {
		writeError(w, http.StatusNotFound, "unknown control population")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if _, err := uuid.Parse(tenantID); err != nil {
		writeError(w, http.StatusUnauthorized, "a valid X-Tenant-Id header is required")
		return
	}

	q := r.URL.Query()
	for k, vs := range q {
		if !paymentAttemptsParams[k] {
			writeError(w, http.StatusBadRequest, "unknown parameter: "+k)
			return
		}
		if len(vs) != 1 {
			writeError(w, http.StatusBadRequest, k+" must be given at most once")
			return
		}
	}

	entity := q.Get("legal_entity_id")
	if _, err := uuid.Parse(entity); err != nil {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required and must be a UUID")
		return
	}

	// period_id is an "as of the end of that month" cut-off, absent meaning none (balance-population semantics).
	var cutOff *time.Time
	if q.Has("period_id") {
		raw := q.Get("period_id")
		month, err := time.Parse("2006-01", raw)
		if err != nil || len(raw) != 7 {
			writeError(w, http.StatusBadRequest, "period_id must be YYYY-MM")
			return
		}
		end := month.AddDate(0, 1, -1)
		cutOff = &end
	}

	limit := defaultPopulationLimit
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n <= 0 || n > maxPopulationLimit {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and "+strconv.Itoa(maxPopulationLimit))
			return
		}
		limit = n
	}

	after := ""
	if q.Has("cursor") {
		b, err := base64.RawURLEncoding.DecodeString(q.Get("cursor"))
		if err == nil {
			_, err = uuid.Parse(string(b))
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "cursor is not valid")
			return
		}
		after = string(b)
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, PaymentInitiationControlPopulationRead); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return
		}
		h.log.Error("control population: authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return
	}

	page, err := h.store.QueryPaymentAttempts(r.Context(), tenantID, domain.PaymentAttemptsQuery{
		LegalEntityID: entity, CutOff: cutOff, Limit: limit, AfterRecordID: after})
	if err != nil {
		if errors.Is(err, domain.ErrControlPopulationTooLarge) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		h.log.Error("GetControlPopulation: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	resp := controlPopulationResponse{Records: page.Records, Watermark: page.Watermark, DeclaredTotals: page.DeclaredTotals}
	if resp.Records == nil {
		resp.Records = []domain.ControlPopulationRecord{}
	}
	if resp.DeclaredTotals.Totals == nil {
		resp.DeclaredTotals.Totals = map[string]string{}
	}
	if page.NextRecordID != "" {
		resp.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.NextRecordID))
	}
	writeJSON(w, http.StatusOK, resp)
}
