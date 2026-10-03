package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// Wave 4 control populations — see the "Wave 4 population definitions"
// section of docs/architecture/control-population-contract.md.

const (
	populationJournalAccountTotals = "journal-account-totals"
	populationTrialBalance         = "trial-balance"

	maxFiscalPeriodLength = 20
	maxCompositeCursorLen = 256
)

// Neither wave-4 population accepts period_id: journal-account-totals is a
// lifetime population and trial-balance is scoped by fiscal_period.
var journalAccountTotalsParams = map[string]bool{
	"legal_entity_id": true,
	"account_codes":   true,
	"normal_balance":  true,
	"limit":           true,
	"cursor":          true,
}

var trialBalancePopulationParams = map[string]bool{
	"legal_entity_id": true,
	"fiscal_period":   true,
	"limit":           true,
	"cursor":          true,
}

// parseCommonPopulationParams validates what every wave-4 population shares:
// the allowed-parameter set (unknown or repeated -> 400), legal_entity_id,
// limit and cursor. The cursor is an opaque base64url of the last record_id,
// which for these populations is composite text rather than a uuid. On failure
// it has already written the 400.
func parseCommonPopulationParams(w http.ResponseWriter, q url.Values, allowed map[string]bool) (entity string, limit int, after string, ok bool) {
	for k, vs := range q {
		if !allowed[k] {
			writeError(w, http.StatusBadRequest, "unknown_parameter", k)
			return
		}
		if len(vs) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_field", k+" must be given at most once")
			return
		}
	}

	entity = q.Get("legal_entity_id")
	if entity == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrLedgerScopeRequired.Error())
		return
	}
	if _, err := uuid.Parse(entity); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	}

	limit = defaultPopulationLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxPopulationLimit {
			writeError(w, http.StatusBadRequest, "invalid_field", "limit must be an integer between 1 and "+strconv.Itoa(maxPopulationLimit))
			return
		}
		limit = n
	}

	if raw := q.Get("cursor"); raw != "" {
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || len(b) == 0 || len(b) > maxCompositeCursorLen || !utf8.Valid(b) || hasControlRune(string(b)) {
			writeError(w, http.StatusBadRequest, "invalid_field", "cursor is not valid")
			return
		}
		after = string(b)
	}
	ok = true
	return
}

func hasControlRune(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// writeControlPopulationPage maps the store outcome to the wire contract.
func (h *Handler) writeControlPopulationPage(w http.ResponseWriter, page *domain.ControlPopulationPage, err error) {
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

func (h *Handler) getJournalAccountTotals(w http.ResponseWriter, r *http.Request, principalID, tenantID string) {
	q := r.URL.Query()
	entity, limit, after, ok := parseCommonPopulationParams(w, q, journalAccountTotalsParams)
	if !ok {
		return
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	page, err := h.store.QueryJournalAccountTotals(r.Context(), tenantID, domain.JournalAccountTotalsQuery{
		LegalEntityID: entity,
		AccountCodes:  codes,
		NormalBalance: normalBalance,
		Limit:         limit,
		AfterRecordID: after,
	})
	h.writeControlPopulationPage(w, page, err)
}

func (h *Handler) getTrialBalancePopulation(w http.ResponseWriter, r *http.Request, principalID, tenantID string) {
	q := r.URL.Query()
	entity, limit, after, ok := parseCommonPopulationParams(w, q, trialBalancePopulationParams)
	if !ok {
		return
	}
	fiscalPeriod := q.Get("fiscal_period")
	if n := utf8.RuneCountInString(fiscalPeriod); n < 1 || n > maxFiscalPeriodLength ||
		!utf8.ValidString(fiscalPeriod) || hasControlRune(fiscalPeriod) {
		writeError(w, http.StatusBadRequest, "invalid_field", "fiscal_period is required (1-20 characters, no control characters)")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	page, err := h.store.QueryTrialBalancePopulation(r.Context(), tenantID, domain.TrialBalancePopulationQuery{
		LegalEntityID: entity,
		FiscalPeriod:  fiscalPeriod,
		Limit:         limit,
		AfterRecordID: after,
	})
	h.writeControlPopulationPage(w, page, err)
}
