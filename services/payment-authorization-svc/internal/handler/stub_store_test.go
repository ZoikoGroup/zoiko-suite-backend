package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payment-authorization-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine and uniqueness semantics.
type stubStore struct {
	auths            map[string]*domain.PaymentAuthorization
	snapshots        map[string][]domain.PayeeSnapshot
	activeByProposal map[string]string // proposal_id -> authorization_id, only while PENDING/APPROVED
	events           map[string][]domain.AuthorizationEvent
}

func newStubStore() *stubStore {
	return &stubStore{
		auths:            map[string]*domain.PaymentAuthorization{},
		snapshots:        map[string][]domain.PayeeSnapshot{},
		activeByProposal: map[string]string{},
		events:           map[string][]domain.AuthorizationEvent{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *stubStore) recordEvent(a *domain.PaymentAuthorization, eventType, detail, actor string) {
	s.events[a.AuthorizationID] = append(s.events[a.AuthorizationID], domain.AuthorizationEvent{
		EventID: uuid.New().String(), TenantID: a.TenantID, AuthorizationID: a.AuthorizationID,
		EventType: eventType, Detail: detail, ActorPrincipalID: actor, CreatedAt: time.Now().UTC(),
	})
}

func (s *stubStore) RequestAuthorization(_ context.Context, tenantID string, auth domain.PaymentAuthorization, snapshots []domain.PayeeSnapshot) (*domain.PaymentAuthorization, error) {
	if _, taken := s.activeByProposal[auth.ProposalID]; taken {
		return nil, domain.ErrProposalAlreadyRequested
	}
	now := time.Now().UTC()
	a := &domain.PaymentAuthorization{
		AuthorizationID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: auth.LegalEntityID,
		ProposalID: auth.ProposalID, ProposalFingerprint: auth.ProposalFingerprint, NetAmount: auth.NetAmount,
		Currency: auth.Currency, Status: domain.StatusPending, RequestedByPrincipalID: auth.RequestedByPrincipalID,
		CreatedAt: now, UpdatedAt: now,
	}
	s.auths[a.AuthorizationID] = a
	s.snapshots[a.AuthorizationID] = snapshots
	s.activeByProposal[auth.ProposalID] = a.AuthorizationID
	s.recordEvent(a, domain.EventAuthorizationRequested, auth.ProposalID, auth.RequestedByPrincipalID)
	return a, nil
}

func (s *stubStore) FindAuthorization(_ context.Context, authorizationID string) (*domain.PaymentAuthorization, error) {
	a, ok := s.auths[authorizationID]
	if !ok {
		return nil, domain.ErrAuthorizationNotFound
	}
	return a, nil
}

func (s *stubStore) ListPayeeSnapshots(_ context.Context, authorizationID string) ([]domain.PayeeSnapshot, error) {
	return s.snapshots[authorizationID], nil
}

func (s *stubStore) freeProposal(a *domain.PaymentAuthorization) {
	if s.activeByProposal[a.ProposalID] == a.AuthorizationID {
		delete(s.activeByProposal, a.ProposalID)
	}
}

func (s *stubStore) ApproveAuthorization(_ context.Context, authorizationID, policyResult, policyVersionID, principalID string) (*domain.PaymentAuthorization, error) {
	a, ok := s.auths[authorizationID]
	if !ok || !domain.CanDecide(a.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	a.Status = domain.StatusApproved
	a.PolicyAssessmentResult = policyResult
	a.PolicyVersionID = policyVersionID
	a.ApprovedByPrincipalID = &principalID
	a.ApprovedAt = &now
	a.UpdatedAt = now
	s.recordEvent(a, domain.EventPaymentAuthorized, "", principalID)
	return a, nil
}

func (s *stubStore) RejectAuthorization(_ context.Context, authorizationID string, req domain.RejectPaymentRequest, principalID string) (*domain.PaymentAuthorization, error) {
	a, ok := s.auths[authorizationID]
	if !ok || !domain.CanDecide(a.Status) {
		return nil, domain.ErrInvalidTransition
	}
	a.Status = domain.StatusRejected
	a.RejectedReason = req.Reason
	a.UpdatedAt = time.Now().UTC()
	s.freeProposal(a)
	s.recordEvent(a, domain.EventAuthorizationRejected, req.Reason, principalID)
	return a, nil
}

func (s *stubStore) InvalidateAuthorization(_ context.Context, authorizationID, reason string) (*domain.PaymentAuthorization, error) {
	a, ok := s.auths[authorizationID]
	if !ok || !(a.Status == domain.StatusPending || a.Status == domain.StatusApproved) {
		return nil, domain.ErrInvalidTransition
	}
	a.Status = domain.StatusInvalidated
	a.InvalidatedReason = reason
	a.UpdatedAt = time.Now().UTC()
	s.freeProposal(a)
	s.recordEvent(a, domain.EventAuthorizationInvalidated, reason, "system")
	return a, nil
}

func (s *stubStore) ConsumeAuthorization(_ context.Context, authorizationID, principalID string) (*domain.PaymentAuthorization, error) {
	a, ok := s.auths[authorizationID]
	if !ok || !domain.CanConsume(a.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	a.Status = domain.StatusConsumed
	a.ConsumedByPrincipalID = &principalID
	a.ConsumedAt = &now
	a.UpdatedAt = now
	s.freeProposal(a)
	s.recordEvent(a, domain.EventAuthorizationConsumed, "", principalID)
	return a, nil
}

func (s *stubStore) RevokeAuthorization(_ context.Context, authorizationID string, req domain.RevokeAuthorizationRequest, principalID string) (*domain.PaymentAuthorization, error) {
	a, ok := s.auths[authorizationID]
	if !ok || !domain.CanRevoke(a.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	a.Status = domain.StatusRevoked
	a.RevokedByPrincipalID = &principalID
	a.RevokedReason = req.Reason
	a.RevokedAt = &now
	a.UpdatedAt = now
	s.freeProposal(a)
	s.recordEvent(a, domain.EventAuthorizationRevoked, req.Reason, principalID)
	return a, nil
}

func (s *stubStore) ExpireAuthorization(_ context.Context, authorizationID, principalID string) (*domain.PaymentAuthorization, error) {
	a, ok := s.auths[authorizationID]
	if !ok || !domain.CanExpire(a.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	a.Status = domain.StatusExpired
	a.ExpiredAt = &now
	a.UpdatedAt = now
	s.freeProposal(a)
	s.recordEvent(a, domain.EventAuthorizationExpired, "", principalID)
	return a, nil
}

func (s *stubStore) ListEvents(_ context.Context, authorizationID string) ([]domain.AuthorizationEvent, error) {
	return s.events[authorizationID], nil
}
