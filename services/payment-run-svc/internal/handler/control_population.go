package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payment-run-svc/internal/authz"
	"zoiko.io/payment-run-svc/internal/domain"
	svcmiddleware "zoiko.io/payment-run-svc/internal/middleware"
)

// ZS-CONTROL-001 §9 control population endpoint — see
// docs/architecture/control-population-contract.md ("payment-run-svc —
// unapplied-supplier-payments").

const (
	// PaymentRunControlPopulationRead is the action the verified human caller
	// needs for the entity to read this population.
	PaymentRunControlPopulationRead = "PAYMENT_RUN_CONTROL_POPULATION_READ"

	defaultPopulationLimit = 1000
	maxPopulationLimit     = 5000
	maxCursorBytes         = 512
)

var unappliedPaymentsParams = map[string]bool{
	"legal_entity_id":  true,
	"unapplied_before": true,
	"limit":            true,
	"cursor":           true,
}

type controlPopulationResponse struct {
	Records        []domain.ControlPopulationRecord `json:"records"`
	NextCursor     string                           `json:"next_cursor"`
	Watermark      string                           `json:"watermark"`
	DeclaredTotals domain.ControlDeclaredTotals     `json:"declared_totals"`
}

// GetUnappliedSupplierPaymentsPopulation serves
// GET /v1/control-populations/unapplied-supplier-payments.
func (h *Handler) GetUnappliedSupplierPaymentsPopulation(w http.ResponseWriter, r *http.Request) {
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
		if !unappliedPaymentsParams[k] {
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
	before := q.Get("unapplied_before")
	if len(before) != len("2006-01-02") {
		writeError(w, http.StatusBadRequest, "unapplied_before is required (YYYY-MM-DD)")
		return
	}
	if _, err := time.Parse("2006-01-02", before); err != nil {
		writeError(w, http.StatusBadRequest, "unapplied_before is required (YYYY-MM-DD)")
		return
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
		if err != nil || len(b) == 0 || len(b) > maxCursorBytes || !utf8.Valid(b) || strings.ContainsRune(string(b), 0) {
			writeError(w, http.StatusBadRequest, "cursor is not valid")
			return
		}
		after = string(b)
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, PaymentRunControlPopulationRead); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return
		}
		h.log.Error("control population: authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return
	}

	page, err := h.store.QueryUnappliedSupplierPayments(r.Context(), tenantID, domain.UnappliedPaymentsQuery{
		LegalEntityID:   entity,
		UnappliedBefore: before,
		Limit:           limit,
		AfterRecordID:   after,
	})
	if err != nil {
		if errors.Is(err, domain.ErrControlPopulationTooLarge) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		h.log.Error("GetUnappliedSupplierPaymentsPopulation: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	resp := controlPopulationResponse{
		Records:        page.Records,
		Watermark:      page.Watermark,
		DeclaredTotals: page.DeclaredTotals,
	}
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
