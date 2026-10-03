// Control-population source endpoint (docs/architecture/control-population-contract.md):
//
//	GET /v1/control-populations/{population}
//
// Read-only. Serves financial-control-svc's population pulls for
// bank-transactions and bank-statements.
package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

// BANKING_CONTROL_POPULATION_READ authorizes reading a legal entity's bank
// data as a control population.
const BANKING_CONTROL_POPULATION_READ = "BANKING_CONTROL_POPULATION_READ"

const (
	controlDefaultLimit = 1000
	controlMaxLimit     = 5000
)

// idPattern bounds legal_entity_id / bank_account_id (VARCHAR(64) columns).
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

var controlAllowedParams = map[string]bool{
	"legal_entity_id": true, "period_id": true, "limit": true, "cursor": true, "bank_account_id": true,
}

type ControlPopulationHandler struct {
	*Handler
	cpStore store.ControlPopulationStore
}

// RegisterControlPopulationRoutes mounts the control-population endpoint.
func RegisterControlPopulationRoutes(r chi.Router, h *Handler, cpStore store.ControlPopulationStore) {
	ch := &ControlPopulationHandler{Handler: h, cpStore: cpStore}
	r.Get("/v1/control-populations/{population}", ch.GetPopulation)
}

func (h *ControlPopulationHandler) GetPopulation(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "missing_tenant_scope")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	population := chi.URLParam(r, "population")
	if population != domain.PopulationBankTransactions && population != domain.PopulationBankStatements {
		writeError(w, http.StatusNotFound, "unknown control population")
		return
	}

	query := r.URL.Query()
	for k, v := range query {
		if !controlAllowedParams[k] {
			writeError(w, http.StatusBadRequest, "unknown query parameter: "+k)
			return
		}
		if len(v) != 1 {
			writeError(w, http.StatusBadRequest, "query parameter must appear once: "+k)
			return
		}
	}

	legalEntityID := query.Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	if !idPattern.MatchString(legalEntityID) {
		writeError(w, http.StatusBadRequest, "legal_entity_id is invalid")
		return
	}

	periodEnd := ""
	if query.Has("period_id") {
		pid := query.Get("period_id")
		start, err := time.Parse("2006-01", pid)
		if err != nil || len(pid) != 7 {
			writeError(w, http.StatusBadRequest, "period_id must be YYYY-MM")
			return
		}
		periodEnd = start.AddDate(0, 1, -1).Format("2006-01-02")
	}

	bankAccountID := ""
	if query.Has("bank_account_id") {
		bankAccountID = query.Get("bank_account_id")
		if !idPattern.MatchString(bankAccountID) {
			writeError(w, http.StatusBadRequest, "bank_account_id is invalid")
			return
		}
	}

	limit := controlDefaultLimit
	if query.Has("limit") {
		n, err := strconv.Atoi(query.Get("limit"))
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
		if limit > controlMaxLimit {
			limit = controlMaxLimit
		}
	}

	after := ""
	if query.Has("cursor") {
		raw, err := base64.RawURLEncoding.DecodeString(query.Get("cursor"))
		if err != nil || len(raw) == 0 || len(raw) > 128 || !utf8.Valid(raw) {
			writeError(w, http.StatusBadRequest, "cursor is invalid")
			return
		}
		after = string(raw)
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, BANKING_CONTROL_POPULATION_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	page, err := h.cpStore.ControlPopulation(r.Context(), domain.ControlPopulationQuery{
		Population: population, TenantID: tenantID, LegalEntityID: legalEntityID,
		PeriodEnd: periodEnd, BankAccountID: bankAccountID, AfterRecordID: after, Limit: limit,
	})
	if err != nil {
		if errors.Is(err, domain.ErrPopulationTooLarge) {
			writeError(w, http.StatusUnprocessableEntity, "control population exceeds the 200000 record cap")
			return
		}
		h.logger.Error("control population read failed", zap.String("population", population), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to read control population")
		return
	}
	if page.NextCursor != "" {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.NextCursor))
	}
	writeJSON(w, http.StatusOK, page)
}
