package handler

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
)

const (
	actionControlPopulationRead = "FINANCIAL_CLOSE_CONTROL_POPULATION_READ"

	defaultPopulationLimit = 1000
	maxPopulationLimit     = 5000
)

var populationEntityIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

// ControlPopulationStore is the read-only store surface of the control population
// endpoint. It is separate from Store so existing Store implementations are untouched;
// a Store that does not implement it makes the endpoint answer 503.
type ControlPopulationStore interface {
	MigrationBatchTieout(ctx context.Context, q domain.MigrationBatchTieoutQuery) (*domain.ControlPopulationPage, error)
}

// GetMigrationBatchTieoutPopulation serves
// GET /v1/control-populations/migration-batch-tieout
// (docs/architecture/control-population-contract.md). Lifetime population: period_id
// is rejected like any other undefined parameter.
func (h *Handler) GetMigrationBatchTieoutPopulation(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	for name, vals := range q {
		switch name {
		case "legal_entity_id", "batch_id", "limit", "cursor":
			if len(vals) != 1 {
				writeError(w, http.StatusBadRequest, "duplicate_parameter", fmt.Sprintf("query parameter %q must appear once", name))
				return
			}
		default:
			writeError(w, http.StatusBadRequest, "unknown_parameter", fmt.Sprintf("query parameter %q is not defined for this population", name))
			return
		}
	}

	legalEntityID := q.Get("legal_entity_id")
	if !populationEntityIDPattern.MatchString(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id is required and must match [A-Za-z0-9][A-Za-z0-9_.:-]{0,63}")
		return
	}
	batchID, err := uuid.Parse(q.Get("batch_id"))
	if err != nil || batchID.String() != q.Get("batch_id") {
		writeError(w, http.StatusBadRequest, "invalid_field", "batch_id is required and must be a lower-case UUID")
		return
	}

	limit := defaultPopulationLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxPopulationLimit {
			writeError(w, http.StatusBadRequest, "invalid_field",
				fmt.Sprintf("limit must be an integer between 1 and %d", maxPopulationLimit))
			return
		}
		limit = n
	}

	after := ""
	if raw := q.Get("cursor"); raw != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_field", "cursor is not valid")
			return
		}
		if _, err := uuid.Parse(string(decoded)); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_field", "cursor is not valid")
			return
		}
		after = string(decoded)
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	cps, ok := h.store.(ControlPopulationStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	page, err := cps.MigrationBatchTieout(r.Context(), domain.MigrationBatchTieoutQuery{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		BatchID:       batchID.String(),
		Limit:         limit,
		AfterRecordID: after,
	})
	if err != nil {
		if errors.Is(err, domain.ErrMigrationBatchNotFound) {
			writeError(w, http.StatusNotFound, "migration_batch_not_found", "")
			return
		}
		h.log.Error("MigrationBatchTieout: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
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
