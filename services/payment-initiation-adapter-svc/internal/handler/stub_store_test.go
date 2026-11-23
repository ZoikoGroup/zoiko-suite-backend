package handler_test

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/middleware"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine and uniqueness semantics.
type stubStore struct {
	attempts          map[string]*domain.PaymentInitiationAttempt
	byIdempotency     map[string]string // platform-wide, like uq_payment_initiation_attempts_idempotency_key
	idempotencyTenant map[string]string // idempotency_key -> owning tenant (RLS-visible lookup)
	events            map[string][]domain.AttemptEvent

	// control-population stub state (control_population_test.go)
	popTenant string
	popQuery  domain.PaymentAttemptsQuery
	popPage   *domain.ControlPopulationPage
	popErr    error
	popCalls  int

}

func newStubStore() *stubStore {
	return &stubStore{
		attempts:          map[string]*domain.PaymentInitiationAttempt{},
		byIdempotency:     map[string]string{},
		idempotencyTenant: map[string]string{},
		events:            map[string][]domain.AttemptEvent{},
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

func (s *stubStore) PrepareAttempt(_ context.Context, tenantID string, req domain.PrepareAttemptRequest, principalID, correlationID string) (*domain.PaymentInitiationAttempt, error) {
	if _, taken := s.byIdempotency[req.IdempotencyKey]; taken {
		return nil, domain.ErrDuplicateIdempotencyKey
	}
	now := time.Now().UTC()
	a := &domain.PaymentInitiationAttempt{
		AttemptID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID,
		SourceReference: req.SourceReference, AuthorizationFingerprint: req.AuthorizationFingerprint,
		AuthorizationID: req.AuthorizationID, AuthorizationSource: req.AuthorizationSource,
		PayerAccountRef: req.PayerAccountRef, PayeeRef: req.PayeeRef, Amount: req.Amount, Currency: req.Currency,
		ExecutionDate: req.ExecutionDate, PaymentReference: req.PaymentReference,
		PayerAccountVerified: req.PayerAccountVerified, IdempotencyKey: req.IdempotencyKey,
		Status: domain.StatusPrepared, CreatedByPrincipalID: principalID, CreatedAt: now, UpdatedAt: now,
	}
	s.attempts[a.AttemptID] = a
	s.byIdempotency[req.IdempotencyKey] = a.AttemptID
	s.idempotencyTenant[req.IdempotencyKey] = tenantID
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

func (s *stubStore) FindByIdempotencyKey(ctx context.Context, idempotencyKey string) (*domain.PaymentInitiationAttempt, error) {
	id, ok := s.byIdempotency[idempotencyKey]
	if !ok || s.idempotencyTenant[idempotencyKey] != middleware.TenantFromContext(ctx) {
		return nil, domain.ErrAttemptNotFound
	}
	return s.attempts[id], nil
}

// ListPaymentAttempts replicates PgStore's tenant+legal-entity scope,
// optional status filter and (created_at, attempt_id) DESC keyset cursor.
func (s *stubStore) ListPaymentAttempts(ctx context.Context, f domain.ListPaymentAttemptsFilter) ([]domain.PaymentAttemptSummary, error) {
	tenantID := middleware.TenantFromContext(ctx)
	var matched []*domain.PaymentInitiationAttempt
	for _, a := range s.attempts {
		if a.LegalEntityID != f.LegalEntityID {
			continue
		}
		if a.TenantID == nil || *a.TenantID != tenantID {
			continue
		}
		if f.Status != "" && string(a.Status) != f.Status {
			continue
		}
		matched = append(matched, a)
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].AttemptID > matched[j].AttemptID
	})
	if f.AfterID != "" {
		cursor, ok := s.attempts[f.AfterID]
		if ok {
			var rest []*domain.PaymentInitiationAttempt
			for _, a := range matched {
				if a.CreatedAt.Before(cursor.CreatedAt) ||
					(a.CreatedAt.Equal(cursor.CreatedAt) && a.AttemptID < cursor.AttemptID) {
					rest = append(rest, a)
				}
			}
			matched = rest
		}
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(matched) > limit {
		matched = matched[:limit]
	}
	out := make([]domain.PaymentAttemptSummary, 0, len(matched))
	for _, a := range matched {
		out = append(out, domain.PaymentAttemptSummary{
			AttemptID: a.AttemptID, LegalEntityID: a.LegalEntityID, SourceReference: a.SourceReference,
			Amount: a.Amount, Currency: a.Currency, Status: a.Status, ExecutionDate: a.ExecutionDate,
			CreatedByPrincipalID: a.CreatedByPrincipalID, SubmittedAt: a.SubmittedAt, ResolvedAt: a.ResolvedAt,
			CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
		})
	}
	return out, nil
}

func (s *stubStore) MarkSubmitted(_ context.Context, attemptID, providerRequestID, responseRef, principalID, _ string) (*domain.PaymentInitiationAttempt, error) {
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

func (s *stubStore) MarkPendingUnknown(_ context.Context, attemptID, principalID, _ string) (*domain.PaymentInitiationAttempt, error) {
	a, ok := s.attempts[attemptID]
	if !ok || !(a.Status == domain.StatusPrepared || a.Status == domain.StatusPendingUnknown) {
		return nil, domain.ErrInvalidTransition
	}
	a.Status = domain.StatusPendingUnknown
	a.UpdatedAt = time.Now().UTC()
	s.recordEvent(a, domain.EventSubmissionAmbiguous, "", principalID)
	return a, nil
}

func (s *stubStore) MarkRejected(_ context.Context, attemptID, reason, principalID, _ string) (*domain.PaymentInitiationAttempt, error) {
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

func (s *stubStore) QuarantineAttempt(_ context.Context, attemptID string, req domain.QuarantineRequest, principalID, _ string) (*domain.PaymentInitiationAttempt, error) {
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

// QueryPaymentAttempts records the query it was given and returns the configured page.
func (s *stubStore) QueryPaymentAttempts(_ context.Context, tenantID string, q domain.PaymentAttemptsQuery) (*domain.ControlPopulationPage, error) {
	s.popCalls++
	s.popTenant, s.popQuery = tenantID, q
	if s.popErr != nil {
		return nil, s.popErr
	}
	if s.popPage != nil {
		return s.popPage, nil
	}
	return &domain.ControlPopulationPage{Watermark: "pa1:n=0;md5=x"}, nil
}
