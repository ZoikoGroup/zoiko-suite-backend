package handler

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/search-indexer-svc/internal/domain"
)

// The critical re-audit finding: POST /v1/restrictions left the ledger at
// epoch 0, so the next ordinary update re-indexed an erased record. The ledger
// is now stamped tombstoned at the restriction's epoch, which is what the
// compare-and-set every later projection passes through refuses against.
func TestRestriction_StampsTheLedgerAtItsEpoch(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/restrictions", map[string]any{
		"scope": "obligation", "source_type": "obligation", "source_id": "ob-1",
		"reason": "PRV_ERASURE", "source_event_id": "erase-1",
	}))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	require.Len(t, h.store.ledger, 1)
	rec := h.store.ledger[0]
	assert.True(t, rec.Tombstoned)
	assert.Positive(t, rec.RestrictionEpoch)
	assert.Equal(t, "obligation", rec.ScopeName)
	assert.Equal(t, "ob-1", rec.SourceID)
	assert.Equal(t, tenantA, rec.TenantID)
}

func TestRestriction_RequiresAScope(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/restrictions", map[string]any{
		"source_type": "obligation", "source_id": "ob-1", "reason": "PRV_ERASURE", "source_event_id": "erase-1",
	}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, h.store.ledger)
}

// READY requires a COMPLETE backfill.
func TestTransitionGeneration_ReadyRequiresACompleteBackfill(t *testing.T) {
	h := newHarness(t, nil)
	h.store.generation.State = domain.GenerationValidating
	h.store.generation.BackfillState = domain.BackfillRunning
	w := do(h, req(t, http.MethodPost, "/v1/index-generations/"+generationID+"/state", map[string]any{"state": "READY"}))
	require.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "backfill_incomplete", bodyOf(t, w)["error"])
}

// The empty-rebuild finding: "engine=0 ledger=4" passed validation and
// activation emptied the scope. An empty generation over a populated ledger is
// now a failed build.
func TestTransitionGeneration_EmptyRebuildOverAPopulatedLedgerFails(t *testing.T) {
	h := newHarness(t, nil)
	h.store.generation.State = domain.GenerationValidating
	h.store.generation.BackfillState = domain.BackfillComplete
	h.engine.count = 0 // the ledger holds 42

	w := do(h, req(t, http.MethodPost, "/v1/index-generations/"+generationID+"/state", map[string]any{"state": "READY"}))
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Equal(t, "ESR-017", bodyOf(t, w)["reason_code"])
	assert.Contains(t, bodyOf(t, w)["detail"], "incomplete build")
}

func TestCreateGeneration_StartsPendingBackfill(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/index-generations", map[string]any{"scope": "obligation"}))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, "PENDING", bodyOf(t, w)["backfill_state"])
	assert.Len(t, h.store.backfillStates, 1)
}

// Publishing v2 does not change what is served: the ACTIVE generation is
// searched with its own (v1) contract until a v2 generation is activated.
func TestSearch_ServesTheActiveGenerationsOwnContract(t *testing.T) {
	h := newHarness(t, nil)
	served := testContract()
	served.State = domain.ContractRetired
	v2 := testContract()
	v2.ContractID = "99999999-9999-9999-9999-999999999999"
	v2.Version = 2
	v2.Fields = append(v2.Fields, domain.SearchFieldDefinition{FieldID: "v2_only", Type: "KEYWORD",
		Returnable: true, SensitivityClass: domain.SensitivityInternal})
	h.store.contract = v2 // the PUBLISHED one
	h.store.contractsByID = map[string]*domain.IndexContract{served.ContractID: served, v2.ContractID: v2}

	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{
		"scope": "obligation", "query": "GST", "requested_fields": []string{"v2_only"},
	}))
	require.Equal(t, http.StatusBadRequest, w.Code, "a v2-only field is not searchable until v2 serves: %s", w.Body.String())
	assert.Equal(t, "ESR-006", bodyOf(t, w)["reason_code"])
}

// Evaluations run inside the caller's own tenant; a case cannot name one.
func TestEvaluateRetrieval_CasesCannotNameATenant(t *testing.T) {
	h := migrationHarness(t)
	w := do(h, req(t, http.MethodPost, "/v1/index-generations/88888888-8888-8888-8888-888888888888/retrieval-evaluations",
		map[string]any{"k": 5, "min_recall": 0.5, "cases": []map[string]any{
			{"tenant_id": tenantB, "query": "overdue", "expected_source_ids": []string{"ob-1"}},
		}}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// NP-41 counts documents that LACK a tenant_id — and only those. The old
// check asked for tenant_id="" through a count that skips empty terms, so it
// counted every live document and refused every populated generation.
func TestValidation_CountsOnlyGenuinelyUntenantedDocuments(t *testing.T) {
	h := newHarness(t, nil)
	h.store.generation.State = domain.GenerationValidating
	h.store.generation.BackfillState = domain.BackfillComplete
	h.engine.count = 42

	w := do(h, req(t, http.MethodPost, "/v1/index-generations/"+generationID+"/state", map[string]any{"state": "READY"}))
	require.Equal(t, http.StatusOK, w.Code, "a populated, tenanted generation passes: %s", w.Body.String())

	h2 := newHarness(t, nil)
	h2.store.generation.State = domain.GenerationValidating
	h2.store.generation.BackfillState = domain.BackfillComplete
	h2.engine.count, h2.engine.missing = 42, 1
	w = do(h2, req(t, http.MethodPost, "/v1/index-generations/"+generationID+"/state", map[string]any{"state": "READY"}))
	require.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, bodyOf(t, w)["detail"], "carry no tenant_id")
}
