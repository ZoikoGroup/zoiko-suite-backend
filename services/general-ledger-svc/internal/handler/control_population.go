package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// ZS-CONTROL-001 §9 control population endpoint — see
// docs/architecture/control-population-contract.md.

const (
	populationAccountPostings = "account-postings"

	defaultPopulationLimit = 1000
	maxPopulationLimit     = 5000

	maxAccountCodes      = 20
	maxAccountCodeLength = 64
)

// accountPostingsParams is the complete set of query parameters the
// account-postings population defines. Anything else is a 400: a misspelt
// filter must never silently widen or narrow a population.
var accountPostingsParams = map[string]bool{
	"legal_entity_id": true,
	"period_id":       true,
	"limit":           true,
	"cursor":          true,
	"account_codes":   true,
	"normal_balance":  true,
}

type controlPopulationResponse struct {
	Records        []domain.ControlPopulationRecord `json:"records"`
	NextCursor     string                           `json:"next_cursor"`
	Watermark      string                           `json:"watermark"`
	DeclaredTotals domain.ControlDeclaredTotals     `json:"declared_totals"`
}

// GetControlPopulation serves GET /v1/control-populations/{population}.
func (h *Handler) GetControlPopulation(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	switch chi.URLParam(r, "population") {
	case populationAccountPostings:
	case populationJournalAccountTotals:
		h.getJournalAccountTotals(w, r, principalID, tenantID)
		return
	case populationJournalBalances, populationControlAccountPostings, populationManualJournals:
		h.getFiscalPeriodPopulation(w, r, principalID, tenantID, chi.URLParam(r, "population"))
		return
	case populationUnpostedEvents:
		h.getUnpostedEvents(w, r, principalID, tenantID)
		return
	case populationEventJournalBreaks:
		h.getEventJournalBreaks(w, r, principalID, tenantID)
		return
	case populationTrialBalance:
		h.getTrialBalancePopulation(w, r, principalID, tenantID)
		return
	default:
		writeError(w, http.StatusNotFound, "unknown_population", "")
		return
	}

	q := r.URL.Query()
	for k, vs := range q {
		if !accountPostingsParams[k] {
			writeError(w, http.StatusBadRequest, "unknown_parameter", k)
			return
		}
		if len(vs) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_field", k+" must be given at most once")
			return
		}
	}

	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrLedgerScopeRequired.Error())
		return
	}
	if _, err := uuid.Parse(legalEntityID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	}

	periodEnd := ""
	if raw := q.Get("period_id"); raw != "" {
		first, err := time.Parse("2006-01", raw)
		if err != nil || len(raw) != 7 {
			writeError(w, http.StatusBadRequest, "invalid_field", "period_id must be YYYY-MM")
			return
		}
		periodEnd = first.AddDate(0, 1, -1).Format("2006-01-02")
	}

	codes, msg := parseAccountCodes(q.Get("account_codes"), q.Has("account_codes"))
	if msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_field", msg)
		return
	}
	normalBalance := q.Get("normal_balance")
	if normalBalance != "DEBIT" && normalBalance != "CREDIT" {
		writeError(w, http.StatusBadRequest, "invalid_field", "normal_balance is required and must be DEBIT or CREDIT")
		return
	}

	limit := defaultPopulationLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxPopulationLimit {
			writeError(w, http.StatusBadRequest, "invalid_field", "limit must be an integer between 1 and "+strconv.Itoa(maxPopulationLimit))
			return
		}
		limit = n
	}

	after := ""
	if raw := q.Get("cursor"); raw != "" {
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err == nil {
			_, err = uuid.Parse(string(b))
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_field", "cursor is not valid")
			return
		}
		after = strings.ToLower(string(b))
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	page, err := h.store.QueryAccountPostings(r.Context(), tenantID, domain.AccountPostingsQuery{
		LegalEntityID: legalEntityID,
		PeriodEnd:     periodEnd,
		AccountCodes:  codes,
		NormalBalance: normalBalance,
		Limit:         limit,
		AfterRecordID: after,
	})
	if err != nil {
		if errors.Is(err, domain.ErrControlPopulationTooLarge) {
			writeError(w, http.StatusUnprocessableEntity, "population_too_large", err.Error())
			return
		}
		h.log.Error("GetControlPopulation: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
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

// parseAccountCodes validates the comma list: 1-20 codes, each non-empty and at
// most 64 characters. Duplicates are collapsed.
func parseAccountCodes(raw string, present bool) ([]string, string) {
	if !present || raw == "" {
		return nil, "account_codes is required (1-20 comma-separated codes)"
	}
	parts := strings.Split(raw, ",")
	if len(parts) > maxAccountCodes {
		return nil, "account_codes accepts at most 20 codes"
	}
	seen := make(map[string]bool, len(parts))
	codes := make([]string, 0, len(parts))
	for _, p := range parts {
		c := strings.TrimSpace(p)
		if c == "" {
			return nil, "account_codes must not contain an empty code"
		}
		if len(c) > maxAccountCodeLength {
			return nil, "each account code must be at most 64 characters"
		}
		if !seen[c] {
			seen[c] = true
			codes = append(codes, c)
		}
	}
	return codes, ""
}
