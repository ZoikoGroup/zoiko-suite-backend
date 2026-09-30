package handler

import (
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// Wave 5 control populations — see the "Wave 5 population definitions"
// section of docs/architecture/control-population-contract.md. None accepts
// period_id: each is scoped by an explicit fiscal_period or created_before.

const (
	populationJournalBalances        = "journal-balances"
	populationControlAccountPostings = "control-account-postings"
	populationUnpostedEvents         = "unposted-events"
	populationManualJournals         = "manual-journals"
)

var fiscalPeriodPopulationParams = map[string]bool{
	"legal_entity_id": true,
	"fiscal_period":   true,
	"limit":           true,
	"cursor":          true,
}

var unpostedEventsParams = map[string]bool{
	"legal_entity_id": true,
	"created_before":  true,
	"limit":           true,
	"cursor":          true,
}

// parseCreatedBefore accepts an RFC3339 timestamp or a YYYY-MM-DD date (taken
// as 00:00:00 UTC of that day) and returns the instant in UTC.
func parseCreatedBefore(raw string) (time.Time, bool) {
	if len(raw) == len("2006-01-02") {
		t, err := time.Parse("2006-01-02", raw)
		return t.UTC(), err == nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func validFiscalPeriod(fp string, q url.Values) bool {
	n := utf8.RuneCountInString(fp)
	return q.Has("fiscal_period") && n >= 1 && n <= maxFiscalPeriodLength && utf8.ValidString(fp) && !hasControlRune(fp)
}

// getFiscalPeriodPopulation serves journal-balances, control-account-postings
// and manual-journals: identical parameters, different store queries.
func (h *Handler) getFiscalPeriodPopulation(w http.ResponseWriter, r *http.Request, principalID, tenantID, population string) {
	q := r.URL.Query()
	entity, limit, after, ok := parseCommonPopulationParams(w, q, fiscalPeriodPopulationParams)
	if !ok {
		return
	}
	fiscalPeriod := q.Get("fiscal_period")
	if !validFiscalPeriod(fiscalPeriod, q) {
		writeError(w, http.StatusBadRequest, "invalid_field", "fiscal_period is required (1-20 characters, no control characters)")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	query := domain.FiscalPeriodPopulationQuery{
		LegalEntityID: entity,
		FiscalPeriod:  fiscalPeriod,
		Limit:         limit,
		AfterRecordID: after,
	}
	var page *domain.ControlPopulationPage
	var err error
	switch population {
	case populationJournalBalances:
		page, err = h.store.QueryJournalBalances(r.Context(), tenantID, query)
	case populationControlAccountPostings:
		page, err = h.store.QueryControlAccountPostings(r.Context(), tenantID, query)
	default:
		page, err = h.store.QueryManualJournals(r.Context(), tenantID, query)
	}
	h.writeControlPopulationPage(w, page, err)
}

func (h *Handler) getUnpostedEvents(w http.ResponseWriter, r *http.Request, principalID, tenantID string) {
	q := r.URL.Query()
	entity, limit, after, ok := parseCommonPopulationParams(w, q, unpostedEventsParams)
	if !ok {
		return
	}
	createdBefore, ok := parseCreatedBefore(q.Get("created_before"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_field", "created_before is required (RFC3339 timestamp or YYYY-MM-DD)")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	page, err := h.store.QueryUnpostedEvents(r.Context(), tenantID, domain.UnpostedEventsQuery{
		LegalEntityID: entity,
		CreatedBefore: createdBefore,
		Limit:         limit,
		AfterRecordID: after,
	})
	h.writeControlPopulationPage(w, page, err)
}

const populationEventJournalBreaks = "event-journal-breaks"

func (h *Handler) getEventJournalBreaks(w http.ResponseWriter, r *http.Request, principalID, tenantID string) {
	q := r.URL.Query()
	entity, limit, after, ok := parseCommonPopulationParams(w, q, unpostedEventsParams)
	if !ok {
		return
	}
	createdBefore, ok := parseCreatedBefore(q.Get("created_before"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_field", "created_before is required (RFC3339 timestamp or YYYY-MM-DD)")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	page, err := h.store.QueryEventJournalBreaks(r.Context(), tenantID, domain.EventJournalBreaksQuery{
		LegalEntityID: entity,
		CreatedBefore: createdBefore,
		Limit:         limit,
		AfterRecordID: after,
	})
	h.writeControlPopulationPage(w, page, err)
}
