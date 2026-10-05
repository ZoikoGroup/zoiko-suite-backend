package handler

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/consolidation-svc/internal/domain"
	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
)

const (
	actionControlPopulationRead = "CONSOLIDATION_CONTROL_POPULATION_READ"

	populationBalanceContributions = "balance-contributions"

	defaultPopulationLimit = 1000
	maxPopulationLimit     = 5000
)

// legalEntityIDPattern: consolidation-svc stores legal entity ids in VARCHAR(255)
// columns and takes them as opaque strings (its own tests use ids like
// "group-1"), so they are not required to be UUIDs. The population endpoint
// accepts the conservative id alphabet used by the other VARCHAR-keyed
// services; any UUID also matches.
var legalEntityIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

// GetControlPopulation serves GET /v1/control-populations/{population}, the
// read-only source side of the control population contract
// (docs/architecture/control-population-contract.md). Only
// `balance-contributions` exists.
//
// Accepted query parameters: legal_entity_id, run_id, limit, cursor - each at
// most once. Anything else (including period_id) is refused: a misspelt filter
// must never silently widen or narrow a population.
func (h *Handler) GetControlPopulation(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if chi.URLParam(r, "population") != populationBalanceContributions {
		writeError(w, http.StatusNotFound, "population_not_found", "")
		return
	}

	q := r.URL.Query()
	for name, vals := range q {
		switch name {
		case "legal_entity_id", "run_id", "limit", "cursor":
			if len(vals) != 1 {
				writeError(w, http.StatusBadRequest, "repeated_parameter", fmt.Sprintf("query parameter %q must be given once", name))
				return
			}
		default:
			writeError(w, http.StatusBadRequest, "unknown_parameter", fmt.Sprintf("query parameter %q is not defined for this population", name))
			return
		}
	}

	legalEntityID := q.Get("legal_entity_id")
	if !legalEntityIDPattern.MatchString(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id is required and must match [A-Za-z0-9][A-Za-z0-9_.:-]{0,63}")
		return
	}
	runID := q.Get("run_id")
	if !isCanonicalUUID(runID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "run_id is required and must be a UUID")
		return
	}

	limit := defaultPopulationLimit
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n <= 0 || n > maxPopulationLimit {
			writeError(w, http.StatusBadRequest, "invalid_field",
				fmt.Sprintf("limit must be an integer between 1 and %d", maxPopulationLimit))
			return
		}
		limit = n
	}

	after := ""
	if q.Has("cursor") {
		decoded, err := base64.RawURLEncoding.DecodeString(q.Get("cursor"))
		if err != nil || !isCanonicalUUID(string(decoded)) {
			writeError(w, http.StatusBadRequest, "invalid_field", "cursor is not valid")
			return
		}
		after = string(decoded)
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	page, err := h.store.BalanceContributionsPopulation(r.Context(), domain.BalanceContributionsQuery{
		TenantID:      tenantID,
		RunID:         runID,
		LegalEntityID: legalEntityID,
		Limit:         limit,
		AfterRecordID: after,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRunNotFound):
			writeError(w, http.StatusNotFound, "run_not_found", "")
		case errors.Is(err, domain.ErrPopulationTooLarge):
			writeError(w, http.StatusUnprocessableEntity, "population_too_large", err.Error())
		default:
			h.log.Error("ControlPopulation: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		}
		return
	}
	if page.Records == nil {
		page.Records = []domain.ControlRecord{}
	}
	if page.DeclaredTotals.Totals == nil {
		page.DeclaredTotals.Totals = map[string]string{}
	}
	if page.NextCursor != "" {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.NextCursor))
	}
	writeJSON(w, http.StatusOK, page)
}

// isCanonicalUUID accepts only the lower-case 36-character hyphenated form, so
// the cursor and run id round-trip exactly.
func isCanonicalUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}
