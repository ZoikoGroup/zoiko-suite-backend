package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

// ZS-CONTROL-001 §9 control population endpoints — see
// docs/architecture/control-population-contract.md ("treasury-svc").

const (
	// actionControlPopulationRead is the action the verified human caller needs, for the entity, to read these populations.
	actionControlPopulationRead = "TREASURY_CONTROL_POPULATION_READ"

	populationFXRateObservations   = "fx-rate-observations"
	populationBankAccountOwnership = "bank-account-ownership"

	defaultPopulationLimit = 1000
	maxPopulationLimit     = 5000
)

var legalEntityIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

var (
	fxRateObservationsParams = map[string]bool{"legal_entity_id": true, "period_id": true, "limit": true, "cursor": true}
	bankOwnershipParams      = map[string]bool{"legal_entity_id": true, "limit": true, "cursor": true}
)

type controlPopulationResponse struct {
	Records        []domain.ControlPopulationRecord `json:"records"`
	NextCursor     string                           `json:"next_cursor"`
	Watermark      string                           `json:"watermark"`
	DeclaredTotals domain.ControlDeclaredTotals     `json:"declared_totals"`
}

// popRequest is the validated common part of a population request.
type popRequest struct {
	principal, tenant, entity, after string
	limit                            int
}

// parsePopulationRequest validates identity and the parameters every population shares, rejecting any
// parameter not in allowed (a misspelt filter must never silently widen or narrow a population).
func (h *Handler) parsePopulationRequest(w http.ResponseWriter, r *http.Request, allowed map[string]bool) (popRequest, url.Values, bool) {
	var pr popRequest
	var ok bool
	if pr.principal, ok = h.requirePrincipal(w, r); !ok {
		return pr, nil, false
	}
	pr.tenant = svcmiddleware.TenantFromContext(r.Context())
	if pr.tenant == "" {
		writeError(w, http.StatusUnauthorized, "tenant_missing", "a valid X-Tenant-Id header is required")
		return pr, nil, false
	}
	q := r.URL.Query()
	for k, vs := range q {
		if !allowed[k] {
			writeError(w, http.StatusBadRequest, "unknown_parameter", k)
			return pr, nil, false
		}
		if len(vs) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_field", k+" must be given at most once")
			return pr, nil, false
		}
	}
	pr.entity = q.Get("legal_entity_id")
	if !legalEntityIDPattern.MatchString(pr.entity) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id is required (1-64 characters: letters, digits, _ . : -)")
		return pr, nil, false
	}
	pr.limit = defaultPopulationLimit
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n <= 0 || n > maxPopulationLimit {
			writeError(w, http.StatusBadRequest, "invalid_field", "limit must be an integer between 1 and "+strconv.Itoa(maxPopulationLimit))
			return pr, nil, false
		}
		pr.limit = n
	}
	if q.Has("cursor") {
		b, err := base64.RawURLEncoding.DecodeString(q.Get("cursor"))
		if err == nil {
			_, err = uuid.Parse(string(b))
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_field", "cursor is not valid")
			return pr, nil, false
		}
		pr.after = string(b)
	}
	return pr, q, true
}

func (h *Handler) writePopulation(w http.ResponseWriter, page *domain.ControlPopulationPage, err error) {
	if err != nil {
		if errors.Is(err, domain.ErrControlPopulationTooLarge) {
			writeError(w, http.StatusUnprocessableEntity, "population_too_large", err.Error())
			return
		}
		h.log.Error("control population: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
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

// GetControlPopulation serves GET /v1/control-populations/{population}.
func (h *Handler) GetControlPopulation(w http.ResponseWriter, r *http.Request) {
	switch chi.URLParam(r, "population") {
	case populationFXRateObservations:
		h.getFXRateObservations(w, r)
	case populationBankAccountOwnership:
		h.getBankAccountOwnership(w, r)
	default:
		writeError(w, http.StatusNotFound, "unknown_population", "")
	}
}

func (h *Handler) getFXRateObservations(w http.ResponseWriter, r *http.Request) {
	pr, q, ok := h.parsePopulationRequest(w, r, fxRateObservationsParams)
	if !ok {
		return
	}
	// A flow population: the month whose rate observations are tested. Required, never defaulted.
	period := q.Get("period_id")
	if t, err := time.Parse("2006-01", period); err != nil || len(period) != 7 || t.IsZero() {
		writeError(w, http.StatusBadRequest, "invalid_field", "period_id is required (YYYY-MM)")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), pr.principal, pr.entity, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	page, err := h.store.QueryFXRateObservations(r.Context(), pr.tenant, domain.FXRateObservationsQuery{
		PeriodID: period, Limit: pr.limit, AfterRecordID: pr.after})
	h.writePopulation(w, page, err)
}

func (h *Handler) getBankAccountOwnership(w http.ResponseWriter, r *http.Request) {
	pr, _, ok := h.parsePopulationRequest(w, r, bankOwnershipParams)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), pr.principal, pr.entity, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	page, err := h.store.QueryBankAccountOwnership(r.Context(), pr.tenant, domain.BankAccountOwnershipQuery{
		LegalEntityID: pr.entity, Limit: pr.limit, AfterRecordID: pr.after})
	h.writePopulation(w, page, err)
}
