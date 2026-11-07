package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/asset-management-svc/internal/domain"
)

// ZS-CONTROL-001 §9 control population endpoint — see
// docs/architecture/control-population-contract.md ("asset-management-svc — depreciation-run-postings").

const (
	// ActionAssetControlPopulationRead is the action the verified human caller needs, for the entity, to read this
	// population.
	ActionAssetControlPopulationRead = "ASSET_CONTROL_POPULATION_READ"

	defaultPopulationLimit = 1000
	maxPopulationLimit     = 5000
)

var (
	assetLegalEntityPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,254}$`)
	depreciationRunPostingsParams = map[string]bool{"legal_entity_id": true, "period_id": true, "limit": true, "cursor": true}
)

// kernelCurrencyProvider is the part of the ledger client that knows the currency depreciation is posted in. A
// client without it, or with the kernel path switched off, cannot say what currency the run was posted in.
type kernelCurrencyProvider interface {
	KernelPostingEnabled() bool
	KernelCurrency() string
}

type controlPopulationResponse struct {
	Records        []domain.ControlPopulationRecord `json:"records"`
	NextCursor     string                           `json:"next_cursor"`
	Watermark      string                           `json:"watermark"`
	DeclaredTotals domain.ControlDeclaredTotals     `json:"declared_totals"`
}

// GetControlPopulation serves GET /v1/control-populations/{population}; only depreciation-run-postings exists.
//
// It answers 422 population_unavailable when depreciation is not posted through the kernel: the register carries no
// currency of its own, and the legacy command posts one journal per run rather than one per category, so neither the
// currency nor the per-category reference would be true.
func (h *Handler) GetControlPopulation(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "population") != domain.PopulationDepreciationRunPostings {
		writeError(w, http.StatusNotFound, "unknown_population", "")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	for k, vs := range q {
		if !depreciationRunPostingsParams[k] {
			writeError(w, http.StatusBadRequest, "unknown_parameter", k)
			return
		}
		if len(vs) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_field", k+" must be given at most once")
			return
		}
	}
	entity := q.Get("legal_entity_id")
	if !assetLegalEntityPattern.MatchString(entity) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id is required (1-255 characters: letters, digits, _ . : -)")
		return
	}
	period := ""
	if q.Has("period_id") {
		period = q.Get("period_id")
		if _, err := time.Parse("2006-01", period); err != nil || len(period) != 7 {
			writeError(w, http.StatusBadRequest, "invalid_field", "period_id must be YYYY-MM")
			return
		}
	}
	limit := defaultPopulationLimit
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n <= 0 || n > maxPopulationLimit {
			writeError(w, http.StatusBadRequest, "invalid_field", "limit must be an integer between 1 and "+strconv.Itoa(maxPopulationLimit))
			return
		}
		limit = n
	}
	after := ""
	if q.Has("cursor") {
		b, err := base64.RawURLEncoding.DecodeString(q.Get("cursor"))
		if err != nil || len(b) == 0 || len(b) > 256 || !utf8.Valid(b) || strings.ContainsRune(string(b), 0) {
			writeError(w, http.StatusBadRequest, "invalid_field", "cursor is not valid")
			return
		}
		after = string(b)
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, ActionAssetControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	kp, ok := h.ledger.(kernelCurrencyProvider)
	if !ok || kp == nil || !kp.KernelPostingEnabled() || strings.TrimSpace(kp.KernelCurrency()) == "" {
		writeError(w, http.StatusUnprocessableEntity, "population_unavailable",
			"depreciation is not posted through the accounting kernel, so the run's currency and per-category reference are unknown")
		return
	}

	page, err := h.store.QueryDepreciationRunPostings(r.Context(), tenantID, domain.DepreciationRunPostingsQuery{
		LegalEntityID: entity, Currency: kp.KernelCurrency(), PeriodID: period, Limit: limit, AfterRecordID: after})
	if err != nil {
		if errors.Is(err, domain.ErrControlPopulationTooLarge) {
			writeError(w, http.StatusUnprocessableEntity, "population_too_large", err.Error())
			return
		}
		h.log.Error("GetControlPopulation: store unavailable", zap.Error(err))
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
