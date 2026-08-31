package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine and uniqueness semantics.
type stubStore struct {
	attempts      map[string]*domain.PaymentInitiationAttempt
	byIdempotency map[string]string // idempotency_key -> attempt_id
	events        map[string][]domain.AttemptEvent
}

func newStubStore() *stubStore {
	return &stubStore{
		attempts:      map[string]*domain.PaymentInitiationAttempt{},
		byIdempotency: map[string]string{},
		events:        map[string][]domain.AttemptEvent{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *stubStore) recordEvent(a *domain.PaymentInitiationAttempt, eventType, detail, actor string) {
	s.events[a.AttemptID] = append(s.events[a.AttemptID], domain.AttemptEvent{
		EventID: uuid.New().String(), TenantID: a.TenantID, AttemptID: a.AttemptID,
		EventType: eventType, Detail: detail, ActorPrincipalID: actor, CreatedAt: time.Now().UTC(),
	})
}

func (s *stubStore) PrepareAttempt(_ context.Context, tenantID string, req domain.PrepareAttemptRequest, principalID string) (*domain.PaymentInitiationAttempt, error) {
	if _, taken := s.byIdempotency[req.IdempotencyKey]; taken {
		return nil, domain.ErrDuplicateIdempotencyKey
	}
	now := time.Now().UTC()
	a := &domain.PaymentInitiationAttempt{
		AttemptID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID,
		SourceReference: req.SourceReference, AuthorizationFingerprint: req.AuthorizationFingerprint,
		PayerAccountRef: req.PayerAccountRef, PayeeRef: req.PayeeRef, Amount: req.Amount, Currency: req.Currency,
		ExecutionDate: req.ExecutionDate, PaymentReference: req.PaymentReference,
		PayerAccountVerified: req.PayerAccountVerified, IdempotencyKey: req.IdempotencyKey,
		Status: domain.StatusPrepared, CreatedByPrincipalID: principalID, CreatedAt: now, UpdatedAt: now,
	}
	s.attempts[a.AttemptID] = a
	s.byIdempotency[req.IdempotencyKey] = a.AttemptID
	s.recordEvent(a, domain.EventInitiationPrepared, "", principalID)
	return a, nil
}

func (s *stubStore) FindAttempt(_ context.Context, attemptID string) (*domain.PaymentInitiationAttempt, error) {
	a, ok := s.attempts[attemptID]
	if !ok {
		return nil, domain.ErrAttemptNotFound
	}
	return a, nil
}

func (s *stubStore) FindByIdempotencyKey(_ context.Context, idempotencyKey string) (*domain.PaymentInitiationAttempt, error) {
	id, ok := s.byIdempotency[idempotencyKey]
	if !ok {
		return nil, domain.ErrAttemptNotFound
	}
	return s.attempts[id], nil
}

func (s *stubStore) MarkSubmitted(_ context.Context, attemptID, providerRequestID, responseRef, principalID string) (*domain.PaymentInitiationAttempt, error) {
	a, ok := s.attempts[attemptID]
	if !ok || !(a.Status == domain.StatusPrepared || a.Status == domain.StatusPendingUnknown) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	a.Status = domain.StatusSubmitted
	a.ProviderRequestID = providerRequestID
	a.ProviderResponseRef = responseRef
	a.SubmittedAt = &now
	a.UpdatedAt = now
	s.recordEvent(a, domain.EventPaymentSubmitted, providerRequestID, principalID)
	return a, nil
}

func (s *stubStore) MarkPendingUnknown(_ context.Context, attemptID, principalID string) (*domain.PaymentInitiationAttempt, error) {
	a, ok := s.attempts[attemptID]
	if !ok || !(a.Status == domain.StatusPrepared || a.Status == domain.StatusPendingUnknown) {
		return nil, domain.ErrInvalidTransition
	}
	a.Status = domain.StatusPendingUnknown
	a.UpdatedAt = time.Now().UTC()
	s.recordEvent(a, domain.EventSubmissionAmbiguous, "", principalID)
	return a, nil
}

func (s *stubStore) MarkRejected(_ context.Context, attemptID, reason, principalID string) (*domain.PaymentInitiationAttempt, error) {
	a, ok := s.attempts[attemptID]
	if !ok || !(a.Status == domain.StatusPrepared || a.Status == domain.StatusPendingUnknown) {
		return nil, domain.ErrInvalidTransition
	}
	a.Status = domain.StatusRejectedBeforeSubmission
	a.RejectionReason = reason
	a.UpdatedAt = time.Now().UTC()
	s.recordEvent(a, domain.EventSubmissionRejected, reason, principalID)
	return a, nil
}

func (s *stubStore) CancelAttempt(_ context.Context, attemptID, principalID string) (*domain.PaymentInitiationAttempt, error) {
	a, ok := s.attempts[attemptID]
	if !ok || a.Status != domain.StatusPrepared {
		return nil, domain.ErrInvalidTransition
	}
	a.Status = domain.StatusCancelled
	a.UpdatedAt = time.Now().UTC()
	s.recordEvent(a, domain.EventAttemptCancelled, "", principalID)
	return a, nil
}

func (s *stubStore) QuarantineAttempt(_ context.Context, attemptID string, req domain.QuarantineRequest, principalID string) (*domain.PaymentInitiationAttempt, error) {
	a, ok := s.attempts[attemptID]
	if !ok || !(a.Status == domain.StatusPrepared || a.Status == domain.StatusPendingUnknown) {
		return nil, domain.ErrInvalidTransition
	}
	a.Status = domain.StatusQuarantined
	a.QuarantineReason = req.Reason
	a.UpdatedAt = time.Now().UTC()
	s.recordEvent(a, domain.EventAttemptQuarantined, req.Reason, principalID)
	return a, nil
}

func (s *stubStore) ResolveAmbiguous(_ context.Context, attemptID string, req domain.ResolveAmbiguousRequest, principalID string) (*domain.PaymentInitiationAttempt, error) {
	a, ok := s.attempts[attemptID]
	if !ok || a.Status != domain.StatusPendingUnknown {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	a.Status = req.ResolvedStatus
	a.AmbiguousResolutionNote = req.Note
	a.ResolvedAt = &now
	a.UpdatedAt = now
	s.recordEvent(a, domain.EventAmbiguousResolved, req.Note, principalID)
	return a, nil
}

func (s *stubStore) ListEvents(_ context.Context, attemptID string) ([]domain.AttemptEvent, error) {
	return s.events[attemptID], nil
}
