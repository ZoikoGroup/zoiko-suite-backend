package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine and uniqueness semantics.
type stubStore struct {
	destinations  map[string]*domain.PayeeDestination
	byFingerprint map[string]string // "party_ref|fingerprint" -> destination_id, for non-superseded rows
	events        map[string][]domain.ChangeEvent
}

func newStubStore() *stubStore {
	return &stubStore{
		destinations:  map[string]*domain.PayeeDestination{},
		byFingerprint: map[string]string{},
		events:        map[string][]domain.ChangeEvent{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func fpKey(partyRef, fingerprint string) string { return partyRef + "|" + fingerprint }

func (s *stubStore) recordEvent(destinationID string, tenantID *string, eventType, detail, actor string) {
	s.events[destinationID] = append(s.events[destinationID], domain.ChangeEvent{
		EventID: uuid.New().String(), TenantID: tenantID, DestinationID: destinationID,
		EventType: eventType, Detail: detail, ActorPrincipalID: actor, CreatedAt: time.Now().UTC(),
	})
}

func (s *stubStore) ProposeDestination(_ context.Context, tenantID string, req domain.ProposeDestinationRequest, principalID string) (*domain.PayeeDestination, error) {
	fingerprint := domain.Fingerprint(req.FinancialInstitution, req.AccountIdentifier, req.Currency)
	key := fpKey(req.PartyRef, fingerprint)
	if _, taken := s.byFingerprint[key]; taken {
		return nil, domain.ErrDuplicateDestination
	}
	scope := req.Scope
	if scope == "" {
		scope = "DEFAULT"
	}
	now := time.Now().UTC()
	d := &domain.PayeeDestination{
		DestinationID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID,
		PartyRef: req.PartyRef, Scope: scope, FinancialInstitution: req.FinancialInstitution,
		AccountIdentifier: req.AccountIdentifier, AccountLast4: domain.Last4(req.AccountIdentifier),
		CountryCode: req.CountryCode, Currency: req.Currency, PayeeName: req.PayeeName, SourceType: req.SourceType,
		Fingerprint: fingerprint, Status: domain.StatusCandidate, ProposedByPrincipalID: principalID,
		CreatedAt: now, UpdatedAt: now,
	}
	s.destinations[d.DestinationID] = d
	s.byFingerprint[key] = d.DestinationID
	s.recordEvent(d.DestinationID, d.TenantID, domain.EventPayeeDestinationProposed, "", principalID)
	return d, nil
}

func (s *stubStore) FindDestination(_ context.Context, destinationID string) (*domain.PayeeDestination, error) {
	d, ok := s.destinations[destinationID]
	if !ok {
		return nil, domain.ErrDestinationNotFound
	}
	return d, nil
}

func (s *stubStore) ListVersions(_ context.Context, partyRef string) ([]domain.PayeeDestination, error) {
	var out []domain.PayeeDestination
	for _, d := range s.destinations {
		if d.PartyRef == partyRef {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (s *stubStore) FindActiveDestination(_ context.Context, partyRef, scope string) (*domain.PayeeDestination, error) {
	for _, d := range s.destinations {
		if d.PartyRef == partyRef && d.Scope == scope && d.Status == domain.StatusActive {
			return d, nil
		}
	}
	return nil, domain.ErrNoActiveDestination
}

func (s *stubStore) VerifyDestination(_ context.Context, destinationID string, req domain.VerifyDestinationRequest, principalID string) (*domain.PayeeDestination, error) {
	d, ok := s.destinations[destinationID]
	if !ok || !domain.CanProposeVerification(d.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	d.Status = domain.StatusVerified
	d.VerificationMethod = req.VerificationMethod
	d.VerificationEvidenceRef = req.VerificationEvidenceRef
	d.VerifiedByPrincipalID = principalID
	d.VerifiedAt = &now
	d.UpdatedAt = now
	s.recordEvent(destinationID, d.TenantID, domain.EventPayeeDestinationVerified, req.VerificationMethod, principalID)
	return d, nil
}

func (s *stubStore) ApproveDestination(_ context.Context, destinationID, principalID string) (*domain.PayeeDestination, error) {
	d, ok := s.destinations[destinationID]
	if !ok || !domain.CanApprove(d.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	d.Status = domain.StatusApprovalPending
	d.ApprovedByPrincipalID = principalID
	d.ApprovedAt = &now
	d.UpdatedAt = now
	s.recordEvent(destinationID, d.TenantID, domain.EventPayeeDestinationApproved, "", principalID)
	return d, nil
}

func (s *stubStore) ActivateDestination(_ context.Context, destinationID, principalID string) (*domain.PayeeDestination, error) {
	d, ok := s.destinations[destinationID]
	if !ok || !domain.CanActivate(d.Status) {
		return nil, domain.ErrInvalidTransition
	}
	for _, other := range s.destinations {
		if other.PartyRef == d.PartyRef && other.Scope == d.Scope && other.Status == domain.StatusActive {
			other.Status = domain.StatusSuperseded
			other.SupersededByDestinationID = destinationID
			other.UpdatedAt = time.Now().UTC()
			s.recordEvent(other.DestinationID, other.TenantID, domain.EventPayeeDestinationSuperseded, "superseded by "+destinationID, principalID)
		}
	}
	d.Status = domain.StatusActive
	d.UpdatedAt = time.Now().UTC()
	s.recordEvent(destinationID, d.TenantID, domain.EventPayeeDestinationActivated, "", principalID)
	return d, nil
}

func (s *stubStore) SuspendDestination(_ context.Context, destinationID string, req domain.SuspendDestinationRequest, principalID string) (*domain.PayeeDestination, error) {
	d, ok := s.destinations[destinationID]
	if !ok || !domain.CanSuspend(d.Status) {
		return nil, domain.ErrInvalidTransition
	}
	d.Status = domain.StatusSuspended
	d.SuspendReason = req.Reason
	d.UpdatedAt = time.Now().UTC()
	s.recordEvent(destinationID, d.TenantID, domain.EventPayeeDestinationSuspended, req.Reason, principalID)
	return d, nil
}

func (s *stubStore) SupersedeDestination(_ context.Context, destinationID string, req domain.SupersedeDestinationRequest, principalID string) (*domain.PayeeDestination, error) {
	d, ok := s.destinations[destinationID]
	if !ok || !domain.CanSupersede(d.Status) {
		return nil, domain.ErrInvalidTransition
	}
	d.Status = domain.StatusSuperseded
	d.UpdatedAt = time.Now().UTC()
	s.recordEvent(destinationID, d.TenantID, domain.EventPayeeDestinationSuperseded, req.Reason, principalID)
	return d, nil
}

func (s *stubStore) ListEvents(_ context.Context, destinationID string) ([]domain.ChangeEvent, error) {
	return s.events[destinationID], nil
}
