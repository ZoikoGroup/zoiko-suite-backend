package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payment-status-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine, ordering, and idempotency
// semantics.
type stubStore struct {
	payments      map[string]*domain.PaymentExecutionState
	appliedEvents map[string]map[string]bool // payment_id -> provider_event_ref -> applied
	events        map[string][]domain.StatusEvent
}

func newStubStore() *stubStore {
	return &stubStore{
		payments:      map[string]*domain.PaymentExecutionState{},
		appliedEvents: map[string]map[string]bool{},
		events:        map[string][]domain.StatusEvent{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *stubStore) recordEvent(p *domain.PaymentExecutionState, eventType, from, to, ref, detail, actor string) {
	s.events[p.PaymentID] = append(s.events[p.PaymentID], domain.StatusEvent{
		EventID: uuid.New().String(), TenantID: p.TenantID, PaymentID: p.PaymentID,
		EventType: eventType, FromStatus: from, ToStatus: to, ProviderEventRef: ref, Detail: detail,
		ActorPrincipalID: actor, CreatedAt: time.Now().UTC(),
	})
}

func (s *stubStore) RecordPaymentStatus(_ context.Context, tenantID string, req domain.RecordPaymentStatusRequest, principalID string) (*domain.PaymentExecutionState, error) {
	now := time.Now().UTC()
	p := &domain.PaymentExecutionState{
		PaymentID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID,
		ProviderRequestID: req.ProviderRequestID, SourceReference: req.SourceReference,
		Status: domain.StatusPrepared, CreatedByPrincipalID: principalID, CreatedAt: now, UpdatedAt: now,
	}
	s.payments[p.PaymentID] = p
	s.appliedEvents[p.PaymentID] = map[string]bool{}
	return p, nil
}

func (s *stubStore) FindPayment(_ context.Context, paymentID string) (*domain.PaymentExecutionState, error) {
	p, ok := s.payments[paymentID]
	if !ok {
		return nil, domain.ErrPaymentNotFound
	}
	return p, nil
}

func (s *stubStore) ApplyCallbackStatus(_ context.Context, paymentID string, payload domain.ProviderCallbackPayload, eventType, finalitySource, actorPrincipalID string) (*domain.PaymentExecutionState, bool, error) {
	p, ok := s.payments[paymentID]
	if !ok {
		return nil, false, domain.ErrPaymentNotFound
	}
	if payload.ProviderEventRef != "" {
		if s.appliedEvents[paymentID][payload.ProviderEventRef] {
			return p, false, nil
		}
		s.appliedEvents[paymentID][payload.ProviderEventRef] = true
	}
	if domain.IsFinal(p.Status) && !(p.Status == domain.StatusSettled && payload.ReportedStatus == domain.StatusReturned) {
		s.recordEvent(p, domain.EventCallbackRejectedRegression, string(p.Status), string(payload.ReportedStatus), payload.ProviderEventRef, "regression blocked", actorPrincipalID)
		return p, false, nil
	}
	p.Status = payload.ReportedStatus
	p.FinalitySource = finalitySource
	p.MappingVersion = payload.MappingVersion
	p.UpdatedAt = time.Now().UTC()
	return p, true, nil
}

func (s *stubStore) LinkStatement(_ context.Context, paymentID string, req domain.LinkStatementRequest, principalID string) (*domain.PaymentExecutionState, bool, error) {
	p, ok := s.payments[paymentID]
	if !ok {
		return nil, false, domain.ErrPaymentNotFound
	}
	if p.Status != req.ReportedStatus {
		p.HasOpenConflict = true
		p.ConflictReason = "statement reports " + string(req.ReportedStatus) + " but canonical status is " + string(p.Status)
		s.recordEvent(p, domain.EventPaymentStatusConflictRaised, string(p.Status), string(req.ReportedStatus), req.StatementReference, p.ConflictReason, principalID)
		return p, true, nil
	}
	return p, false, nil
}

func (s *stubStore) ResolveConflict(_ context.Context, paymentID string, req domain.ResolveConflictRequest, principalID string) (*domain.PaymentExecutionState, error) {
	p, ok := s.payments[paymentID]
	if !ok || !p.HasOpenConflict {
		return nil, domain.ErrInvalidTransition
	}
	from := p.Status
	p.Status = req.FinalStatus
	p.HasOpenConflict = false
	p.ConflictReason = ""
	p.FinalitySource = "MANUAL_OVERRIDE"
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventPaymentStatusConflictResolved, string(from), string(req.FinalStatus), "", req.Reason, principalID)
	return p, nil
}

func (s *stubStore) RecordReturn(_ context.Context, paymentID string, req domain.RecordReturnRequest, principalID string) (*domain.PaymentExecutionState, error) {
	p, ok := s.payments[paymentID]
	if !ok || !domain.CanReturn(p.Status) {
		return nil, domain.ErrInvalidTransition
	}
	if req.ProviderEventRef != "" {
		if s.appliedEvents[paymentID][req.ProviderEventRef] {
			return nil, domain.ErrInvalidTransition
		}
		s.appliedEvents[paymentID][req.ProviderEventRef] = true
	}
	p.Status = domain.StatusReturned
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventPaymentReturned, "SETTLED", "RETURNED", req.ProviderEventRef, req.Reason, principalID)
	return p, nil
}

func (s *stubStore) CancelPayment(_ context.Context, paymentID string, req domain.CancelRequest, principalID string) (*domain.PaymentExecutionState, error) {
	p, ok := s.payments[paymentID]
	if !ok || !domain.CanCancel(p.Status) {
		return nil, domain.ErrInvalidTransition
	}
	p.Status = domain.StatusCancelled
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventPaymentCancelled, "", "CANCELLED", "", req.Reason, principalID)
	return p, nil
}

func (s *stubStore) ListEvents(_ context.Context, paymentID string) ([]domain.StatusEvent, error) {
	return s.events[paymentID], nil
}

func (s *stubStore) ListUnresolved(_ context.Context) ([]domain.PaymentExecutionState, error) {
	var out []domain.PaymentExecutionState
	for _, p := range s.payments {
		if p.HasOpenConflict {
			out = append(out, *p)
		}
	}
	return out, nil
}
