package handler_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// GetInputBatchByRun answers the batch a run consumed: only a CONSUMED batch linked to the run counts.
func (s *stubResultStore) GetInputBatchByRun(_ context.Context, runID string) (*domain.InputBatch, error) {
	for _, b := range s.batches {
		if b.State == domain.InputBatchConsumed && b.PayrollRunID != nil && *b.PayrollRunID == runID {
			return b, nil
		}
	}
	return nil, nil
}

func consumedBatchFor(run *domain.PayrollRun) *domain.InputBatch {
	id := run.RunID
	return &domain.InputBatch{BatchID: uuid.NewString(), LegalEntityID: run.LegalEntityID, PayrollPeriodID: "2026-09", State: domain.InputBatchConsumed, PayrollRunID: &id}
}

func TestWFP04_ARunThatConsumedAFrozenBatchLinksItAndIsNotLabelledNotFrozen(t *testing.T) {
	st, run := zoikoFixture()
	b := consumedBatchFor(run)
	st.batches[b.BatchID] = b
	rr := resDo(newResultRouter(st, &stubAuthZ{}, &stubResultWorkers{}), "POST", resBase+"/from-run", fromRunBody(run), "p-1")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	req := st.received[0].Request
	require.NotNil(t, req.InputBatchID)
	assert.Equal(t, b.BatchID, *req.InputBatchID, "the result links the batch the calculation consumed")
	assert.NotContains(t, req.EvidenceFlags, domain.FlagInputNotFrozen)
	assert.Contains(t, req.EvidenceFlags, domain.FlagPlaceholderTaxRate, "frozen inputs say nothing about where the tax came from")
}

func TestWFP04_NamingADifferentBatchThanTheOneTheRunConsumedIsRefused(t *testing.T) {
	st, run := zoikoFixture()
	b := consumedBatchFor(run)
	st.batches[b.BatchID] = b
	body := fromRunBody(run)
	body["input_batch_id"] = uuid.NewString()
	rr := resDo(newResultRouter(st, &stubAuthZ{}, &stubResultWorkers{}), "POST", resBase+"/from-run", body, "p-1")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "INPUT_BATCH_MISMATCH", resCode(t, rr))
	assert.Empty(t, st.received)

	body["input_batch_id"] = b.BatchID
	rr = resDo(newResultRouter(st, &stubAuthZ{}, &stubResultWorkers{}), "POST", resBase+"/from-run", body, "p-1")
	assert.Equal(t, http.StatusCreated, rr.Code, "naming the consumed batch itself is fine")
}

func TestWFP04_ARunThatConsumedNoBatchIsLabelledNotFrozenEvenIfOneIsNamed(t *testing.T) {
	st, run := zoikoFixture()
	named := &domain.InputBatch{BatchID: uuid.NewString(), LegalEntityID: run.LegalEntityID, PayrollPeriodID: "2026-09", State: domain.InputBatchFrozen}
	st.batches[named.BatchID] = named
	body := fromRunBody(run)
	body["input_batch_id"] = named.BatchID
	rr := resDo(newResultRouter(st, &stubAuthZ{}, &stubResultWorkers{}), "POST", resBase+"/from-run", body, "p-1")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	req := st.received[0].Request
	assert.Contains(t, req.EvidenceFlags, domain.FlagInputNotFrozen, "the calculation did not use it; linking a batch is not the same as computing from it")
	require.NotNil(t, req.InputBatchID)
	assert.Equal(t, named.BatchID, *req.InputBatchID, "the link the caller asked for is still recorded and checked")
}
