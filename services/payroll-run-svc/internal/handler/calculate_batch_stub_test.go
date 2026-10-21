package handler_test

import (
	"context"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

func (s *stubStore) GetInputBatch(_ context.Context, id string) (*domain.InputBatch, error) {
	if b, ok := s.batches[id]; ok {
		return b, nil
	}
	return nil, domain.ErrInputBatchNotFound
}

// GetInputBatchByRun answers the batch a run consumed: only a CONSUMED batch counts.
func (s *stubStore) GetInputBatchByRun(_ context.Context, runID string) (*domain.InputBatch, error) {
	for _, b := range s.batches {
		if b.State == domain.InputBatchConsumed && b.PayrollRunID != nil && *b.PayrollRunID == runID {
			return b, nil
		}
	}
	return nil, nil
}

func (s *stubStore) ListInputLines(_ context.Context, batchID, state, _ string) ([]domain.InputLine, error) {
	var out []domain.InputLine
	for _, l := range s.batchLines[batchID] {
		if state == "" || l.State == state {
			out = append(out, l)
		}
	}
	return out, nil
}

func (s *stubStore) ConsumeInputBatch(_ context.Context, id string, _ int, runID string, _ store.InputEventBuilder) (*domain.InputBatch, error) {
	if s.consumeErr != nil {
		return nil, s.consumeErr
	}
	b, ok := s.batches[id]
	if !ok {
		return nil, domain.ErrInputBatchNotFound
	}
	if b.State != domain.InputBatchFrozen {
		return nil, domain.ErrInputBatchNotFrozen
	}
	b.State, b.PayrollRunID = domain.InputBatchConsumed, &runID
	s.consumed = append(s.consumed, id)
	return b, nil
}
