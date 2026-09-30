package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/tax-authority-interface-svc/internal/authz"
	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
	"zoiko.io/tax-authority-interface-svc/internal/store"
)

// ZS-CONTROL-001 §9 control population endpoint — see
// docs/architecture/control-population-contract.md.

const (
	ActionControlPopulationRead = "TAX_AUTHORITY_CONTROL_POPULATION_READ"

	populationUnacknowledgedFilings = "unacknowledged-filings"

	defaultPopulationLimit = 1000
	maxPopulationLimit     = 5000
	maxCursorLen           = 128
)

// unacknowledgedFilingsParams is the complete parameter set of the population.
// period_id is deliberately absent: the population is defined by
// submitted_before, and accepting a period would silently do nothing.
var unacknowledgedFilingsParams = map[string]bool{
	"legal_entity_id":  true,
	"limit":            true,
	"cursor":           true,
	"submitted_before": true,
}

var entityIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

type controlPopulationResponse struct {
	Records        []domain.ControlPopulationRecord `json:"records"`
	NextCursor     string                           `json:"next_cursor"`
	Watermark      string                           `json:"watermark"`
	DeclaredTotals domain.ControlDeclaredTotals     `json:"declared_totals"`
}

func (h *Handler) registerControlPopulationRoutes(r chi.Router) {
	r.Get("/v1/control-populations/{population}", h.GetControlPopulation)
}

// GetControlPopulation serves GET /v1/control-populations/{population}.
func (h *Handler) GetControlPopulation(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get(principalIDHeader)
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "missing X-Principal-Id header")
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "missing tenant scope")
		return
	}
	if chi.URLParam(r, "population") != populationUnacknowledgedFilings {
		writeError(w, http.StatusNotFound, "unknown population")
		return
	}

	q := r.URL.Query()
	for k, vs := range q {
		if !unacknowledgedFilingsParams[k] {
			writeError(w, http.StatusBadRequest, "unknown parameter: "+k)
			return
		}
		if len(vs) != 1 {
			writeError(w, http.StatusBadRequest, k+" must be given at most once")
			return
		}
	}

	entity := q.Get("legal_entity_id")
	if entity == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	if !entityIDPattern.MatchString(entity) {
		writeError(w, http.StatusBadRequest, "legal_entity_id is not valid")
		return
	}

	before, ok := parseSubmittedBefore(q.Get("submitted_before"))
	if !ok {
		writeError(w, http.StatusBadRequest, "submitted_before is required (RFC3339 timestamp or YYYY-MM-DD)")
		return
	}

	limit := defaultPopulationLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxPopulationLimit {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and "+strconv.Itoa(maxPopulationLimit))
			return
		}
		limit = n
	}

	after := ""
	if raw := q.Get("cursor"); raw != "" {
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || len(b) == 0 || len(b) > maxCursorLen || !utf8.Valid(b) || hasControlRune(string(b)) {
			writeError(w, http.StatusBadRequest, "cursor is not valid")
			return
		}
		after = string(b)
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, ActionControlPopulationRead); err != nil {
		if errors.Is(err, authz.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to read control populations")
			return
		}
		h.logger.Error("control population: authorization check failed — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization check failed")
		return
	}

	reader, ok := h.store.(store.ControlPopulationReader)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "control populations are not available on this store")
		return
	}
	page, err := reader.QueryUnacknowledgedFilings(r.Context(), tenantID, domain.UnacknowledgedFilingsQuery{
		LegalEntityID:   entity,
		SubmittedBefore: before,
		Limit:           limit,
		AfterRecordID:   after,
	})
	if err != nil {
		if errors.Is(err, domain.ErrControlPopulationTooLarge) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		h.logger.Error("control population: store unavailable", zap.Error(err))
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

// parseSubmittedBefore accepts RFC3339 (converted to UTC) or YYYY-MM-DD (read as
// 00:00 UTC).
func parseSubmittedBefore(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	if len(raw) == len("2006-01-02") {
		t, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return time.Time{}, false
		}
		return t.UTC(), true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func hasControlRune(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
