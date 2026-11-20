package handler

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// migration-batch-tieout — see docs/architecture/control-population-contract.md,
// "financial-close-svc — Wave 6". financial-close-svc's own migration-batch
// write path has no real callers left and is slated for retirement; this is
// the equivalent population sourced from general-ledger-svc's own kernel
// migration batches (internal/kernel/migration.go), the real, spec-compliant
// posting path migration-integrity-svc now drives. Same wire contract, same
// authz action as every other general-ledger-svc population
// (actionControlPopulationRead = GL_CONTROL_POPULATION_READ) — only the URL a
// consumer calls changes, not the shape or the checking logic reading it.
const populationMigrationBatchTieout = "migration-batch-tieout"

var migrationBatchTieoutParams = map[string]bool{
	"legal_entity_id": true,
	"batch_id":        true,
	"limit":           true,
	"cursor":          true,
}

// getMigrationBatchTieout serves GET
// /v1/control-populations/migration-batch-tieout. Lifetime population,
// exactly one record for one batch: period_id is rejected like any other
// undefined parameter (it is simply absent from migrationBatchTieoutParams).
func (h *Handler) getMigrationBatchTieout(w http.ResponseWriter, r *http.Request, principalID, tenantID string) {
	q := r.URL.Query()
	entity, limit, after, ok := parseCommonPopulationParams(w, q, migrationBatchTieoutParams)
	if !ok {
		return
	}

	rawBatch := q.Get("batch_id")
	batchID, err := uuid.Parse(rawBatch)
	if err != nil || batchID.String() != rawBatch {
		writeError(w, http.StatusBadRequest, "invalid_field", "batch_id is required and must be a lower-case UUID")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	page, err := h.store.QueryMigrationBatchTieout(r.Context(), tenantID, domain.MigrationBatchTieoutQuery{
		LegalEntityID: entity,
		BatchID:       batchID.String(),
		Limit:         limit,
		AfterRecordID: after,
	})
	if err != nil {
		if errors.Is(err, domain.ErrMigrationBatchNotFound) {
			writeError(w, http.StatusNotFound, "migration_batch_not_found", "")
			return
		}
	}
	h.writeControlPopulationPage(w, page, err)
}
