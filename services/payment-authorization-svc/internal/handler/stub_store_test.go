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
	signatures       map[string][]domain.Signature
	idem             map[string]*domain.IdempotencyRecord
}

func newStubStore() *stubStore {
	return &stubStore{
		auths:            map[string]*domain.PaymentAuthorization{},
		snapshots:        map[string][]domain.PayeeSnapshot{},
		activeByProposal: map[string]string{},
		events:           map[string][]domain.AuthorizationEvent{},
		signatures:       map[string][]domain.Signature{},
		idem:             map[string]*domain.IdempotencyRecord{},
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
		CreatedAt: now, UpdatedAt: now, Version: 1, RequiredSignatures: 1, ExpiresAt: auth.ExpiresAt,
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

// ApproveAuthorization mirrors PgStore: one signature per principal, APPROVED
// only once the (never-decreasing) required count is reached.
func (s *stubStore) ApproveAuthorization(_ context.Context, authorizationID, policyResult, policyVersionID, principalID string, requiredSignatures int, expectedVersion *int) (*domain.PaymentAuthorization, error) {
	a, ok := s.auths[authorizationID]
	if !ok || !domain.CanDecide(a.Status) {
		return nil, domain.ErrInvalidTransition
	}
	if expectedVersion != nil && a.Version != *expectedVersion {
		return nil, domain.ErrStaleVersion
	}
	if a.ExpiresAt != nil && !a.ExpiresAt.After(time.Now()) {
		return nil, domain.ErrAuthorizationExpired
	}
	for _, sg := range s.signatures[authorizationID] {
		if sg.SignerPrincipalID == principalID {
			return nil, domain.ErrAlreadySigned
		}
	}
	now := time.Now().UTC()
	s.signatures[authorizationID] = append(s.signatures[authorizationID], domain.Signature{
		SignatureID: uuid.New().String(), AuthorizationID: authorizationID, SignerPrincipalID: principalID,
		PolicyResult: policyResult, PolicyVersionID: policyVersionID, SignedAt: now,
	})
	if requiredSignatures > a.RequiredSignatures {
		a.RequiredSignatures = requiredSignatures
	}
	a.SignatureCount++
	a.PolicyAssessmentResult = policyResult
	a.PolicyVersionID = policyVersionID
	a.UpdatedAt = now
	a.Version++
	if a.SignatureCount < a.RequiredSignatures {
		s.recordEvent(a, domain.EventAuthorizationSigned, "", principalID)
		return a, nil
	}
	a.Status = domain.StatusApproved
	a.ApprovedByPrincipalID = &principalID
	a.ApprovedAt = &now
	s.recordEvent(a, domain.EventPaymentAuthorized, "", principalID)
	return a, nil
}

func (s *stubStore) ListSignatures(_ context.Context, authorizationID string) ([]domain.Signature, error) {
	return s.signatures[authorizationID], nil
}

func (s *stubStore) ListExpiredCandidates(_ context.Context, limit int) ([]domain.ExpiredRef, error) {
	var out []domain.ExpiredRef
	for _, a := range s.auths {
		if domain.CanExpire(a.Status) && a.ExpiresAt != nil && a.ExpiresAt.Before(time.Now()) && len(out) < limit {
			tenant := ""
			if a.TenantID != nil {
				tenant = *a.TenantID
			}
			out = append(out, domain.ExpiredRef{AuthorizationID: a.AuthorizationID, TenantID: tenant})
		}
	}
	return out, nil
}

func idemKey(scope, key string) string { return scope + "|" + key }

func (s *stubStore) BeginIdempotent(_ context.Context, scope, key, requestHash string) (*domain.IdempotencyRecord, bool, error) {
	if rec, ok := s.idem[idemKey(scope, key)]; ok {
		cp := *rec
		return &cp, false, nil
	}
	s.idem[idemKey(scope, key)] = &domain.IdempotencyRecord{RequestHash: requestHash}
	return nil, true, nil
}

func (s *stubStore) CompleteIdempotent(_ context.Context, scope, key string, statusCode int, body []byte) error {
	if rec, ok := s.idem[idemKey(scope, key)]; ok && !rec.Completed {
		rec.Completed, rec.StatusCode, rec.Body = true, statusCode, append([]byte(nil), body...)
	}
	return nil
}

func (s *stubStore) ReleaseIdempotent(_ context.Context, scope, key string) error {
	if rec, ok := s.idem[idemKey(scope, key)]; ok && !rec.Completed {
		delete(s.idem, idemKey(scope, key))
	}
	return nil
}

func (s *stubStore) PurgeIdempotency(context.Context, time.Duration) (int64, error) { return 0, nil }

func (s *stubStore) RejectAuthorization(_ context.Context, authorizationID string, req domain.RejectPaymentRequest, principalID string) (*domain.PaymentAuthorization, error) {
	a, ok := s.auths[authorizationID]
	if !ok || !domain.CanDecide(a.Status) {
		return nil, domain.ErrInvalidTransition
	}
	a.Status = domain.StatusRejected
	a.RejectedReason = req.Reason
	a.UpdatedAt = time.Now().UTC()
	a.Version++
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
	a.Version++
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
	a.Version++
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
	a.Version++
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
	a.Version++
	s.freeProposal(a)
	s.recordEvent(a, domain.EventAuthorizationExpired, "", principalID)
	return a, nil
}

func (s *stubStore) ListEvents(_ context.Context, authorizationID string) ([]domain.AuthorizationEvent, error) {
	return s.events[authorizationID], nil
}
