package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payment-run-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine and uniqueness semantics.
type stubStore struct {
	runs              map[string]*domain.PaymentRun
	instructions      map[string]*domain.RunInstruction
	instructionsByRun map[string][]string
	consumedAuth      map[string]string          // authorization_id -> instruction_id, once ever consumed
	reconciledEvents  map[string]map[string]bool // instruction_id -> provider_event_ref -> applied
	events            map[string][]domain.RunEvent
}

func newStubStore() *stubStore {
	return &stubStore{
		runs:              map[string]*domain.PaymentRun{},
		instructions:      map[string]*domain.RunInstruction{},
		instructionsByRun: map[string][]string{},
		consumedAuth:      map[string]string{},
		reconciledEvents:  map[string]map[string]bool{},
		events:            map[string][]domain.RunEvent{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *stubStore) recordEvent(runID string, tenantID *string, eventType, detail, actor string) {
	s.events[runID] = append(s.events[runID], domain.RunEvent{
		EventID: uuid.New().String(), TenantID: tenantID, RunID: runID,
		EventType: eventType, Detail: detail, ActorPrincipalID: actor, CreatedAt: time.Now().UTC(),
	})
}

func (s *stubStore) CreateRun(_ context.Context, tenantID string, req domain.CreateRunRequest, instructions []domain.RunInstruction, principalID string) (*domain.PaymentRun, []domain.RunInstruction, error) {
	for _, ins := range instructions {
		if _, taken := s.consumedAuth[ins.AuthorizationID]; taken {
			return nil, nil, domain.ErrAuthorizationNotEligible
		}
	}
	now := time.Now().UTC()
	run := &domain.PaymentRun{
		RunID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID,
		PayingBankAccountRef: req.PayingBankAccountRef, Currency: req.Currency, ValueDate: req.ValueDate,
		PaymentMethod: req.PaymentMethod, Status: domain.StatusDraft, CreatedByPrincipalID: principalID,
		CreatedAt: now, UpdatedAt: now,
	}
	s.runs[run.RunID] = run
	var out []domain.RunInstruction
	for _, ins := range instructions {
		created := domain.RunInstruction{
			InstructionID: uuid.New().String(), TenantID: run.TenantID, RunID: run.RunID,
			AuthorizationID: ins.AuthorizationID, PayeeRef: ins.PayeeRef, NetAmount: ins.NetAmount,
			Currency: ins.Currency, Status: domain.InstructionPending, CreatedAt: now,
		}
		s.instructions[created.InstructionID] = &created
		s.instructionsByRun[run.RunID] = append(s.instructionsByRun[run.RunID], created.InstructionID)
		s.consumedAuth[ins.AuthorizationID] = created.InstructionID
		out = append(out, created)
	}
	s.recordEvent(run.RunID, run.TenantID, domain.EventRunCreated, "", principalID)
	return run, out, nil
}

func (s *stubStore) FindRun(_ context.Context, runID string) (*domain.PaymentRun, error) {
	r, ok := s.runs[runID]
	if !ok {
		return nil, domain.ErrRunNotFound
	}
	return r, nil
}

func (s *stubStore) ListInstructions(_ context.Context, runID string) ([]domain.RunInstruction, error) {
	var out []domain.RunInstruction
	for _, id := range s.instructionsByRun[runID] {
		out = append(out, *s.instructions[id])
	}
	return out, nil
}

func (s *stubStore) FindInstruction(_ context.Context, instructionID string) (*domain.RunInstruction, error) {
	i, ok := s.instructions[instructionID]
	if !ok {
		return nil, domain.ErrInstructionNotFound
	}
	return i, nil
}

func (s *stubStore) ValidateRun(_ context.Context, runID, principalID string) (*domain.PaymentRun, error) {
	r, ok := s.runs[runID]
	if !ok || !domain.CanValidate(r.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	r.Status = domain.StatusValidated
	r.ValidatedAt = &now
	r.UpdatedAt = now
	s.recordEvent(runID, r.TenantID, domain.EventRunValidated, "", principalID)
	return r, nil
}

func (s *stubStore) MarkInstructionConsumed(_ context.Context, instructionID string) error {
	i, ok := s.instructions[instructionID]
	if !ok {
		return domain.ErrInstructionNotFound
	}
	now := time.Now().UTC()
	i.ConsumedAt = &now
	return nil
}

func (s *stubStore) LockRun(_ context.Context, runID, principalID string) (*domain.PaymentRun, error) {
	r, ok := s.runs[runID]
	if !ok || !domain.CanLock(r.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	r.Status = domain.StatusLocked
	r.LockedAt = &now
	r.UpdatedAt = now
	s.recordEvent(runID, r.TenantID, domain.EventRunLocked, "", principalID)
	return r, nil
}

func (s *stubStore) MarkRunException(_ context.Context, runID, reason, principalID string) (*domain.PaymentRun, error) {
	r, ok := s.runs[runID]
	if !ok || r.Status == domain.StatusCompleted || r.Status == domain.StatusCancelled {
		return nil, domain.ErrInvalidTransition
	}
	r.Status = domain.StatusException
	r.ExceptionReason = reason
	r.UpdatedAt = time.Now().UTC()
	s.recordEvent(runID, r.TenantID, domain.EventRunExceptionRaised, reason, principalID)
	return r, nil
}

func (s *stubStore) SubmitRun(_ context.Context, runID, idempotencyKey, principalID string) (*domain.PaymentRun, error) {
	r, ok := s.runs[runID]
	if !ok || !domain.CanSubmit(r.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	r.Status = domain.StatusSubmitted
	r.IdempotencyKey = idempotencyKey
	r.SubmittedAt = &now
	r.UpdatedAt = now
	s.recordEvent(runID, r.TenantID, domain.EventRunSubmitted, "", principalID)
	return r, nil
}

func (s *stubStore) CancelRun(_ context.Context, runID string, req domain.CancelRunRequest, principalID string) (*domain.PaymentRun, error) {
	r, ok := s.runs[runID]
	if !ok || !domain.CanCancel(r.Status) {
		return nil, domain.ErrInvalidTransition
	}
	r.Status = domain.StatusCancelled
	r.CancelReason = req.Reason
	r.UpdatedAt = time.Now().UTC()
	s.recordEvent(runID, r.TenantID, domain.EventRunCancelled, req.Reason, principalID)
	return r, nil
}

func (s *stubStore) CloseRun(_ context.Context, runID string, req domain.CloseRunRequest, principalID string) (*domain.PaymentRun, error) {
	r, ok := s.runs[runID]
	if !ok || !domain.CanClose(r.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	r.Status = domain.StatusCompleted
	r.CloseNote = req.Note
	r.ClosedAt = &now
	r.UpdatedAt = now
	s.recordEvent(runID, r.TenantID, domain.EventRunCompleted, req.Note, principalID)
	return r, nil
}

func (s *stubStore) ReconcileInstruction(_ context.Context, req domain.ReconcileInstructionRequest, principalID string) (*domain.RunInstruction, bool, error) {
	i, ok := s.instructions[req.InstructionID]
	if !ok {
		return nil, false, domain.ErrInstructionNotFound
	}
	if s.reconciledEvents[req.InstructionID] == nil {
		s.reconciledEvents[req.InstructionID] = map[string]bool{}
	}
	if s.reconciledEvents[req.InstructionID][req.ProviderEventRef] {
		return i, false, nil
	}
	s.reconciledEvents[req.InstructionID][req.ProviderEventRef] = true
	i.Status = req.ExternalStatus
	i.ProviderEventRef = req.ProviderEventRef
	return i, true, nil
}

func (s *stubStore) UpdateRunAggregateStatus(_ context.Context, runID string, newStatus domain.RunStatus, principalID string) (*domain.PaymentRun, error) {
	r, ok := s.runs[runID]
	allowed := map[domain.RunStatus]bool{domain.StatusSubmitted: true, domain.StatusAccepted: true, domain.StatusRejected: true, domain.StatusPartiallyAccepted: true}
	if !ok || !allowed[r.Status] {
		return nil, domain.ErrInvalidTransition
	}
	r.Status = newStatus
	r.UpdatedAt = time.Now().UTC()
	s.recordEvent(runID, r.TenantID, "PAYMENT_RUN_STATUS_"+string(newStatus), "", principalID)
	return r, nil
}

func (s *stubStore) RetryInstruction(_ context.Context, instructionID, principalID string) error {
	i, ok := s.instructions[instructionID]
	if !ok {
		return domain.ErrInstructionNotFound
	}
	s.recordEvent(i.RunID, i.TenantID, domain.EventInstructionRetried, instructionID, principalID)
	return nil
}

func (s *stubStore) ListEvents(_ context.Context, runID string) ([]domain.RunEvent, error) {
	return s.events[runID], nil
}
