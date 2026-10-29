package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine, uniqueness and command-
// idempotency semantics.
type stubStore struct {
	destinations  map[string]*domain.PayeeDestination
	byFingerprint map[string]string // "party_ref|fingerprint" -> destination_id, for non-superseded rows
	events        map[string][]domain.ChangeEvent
	idempotency   map[string]stubClaim
}

type stubClaim struct {
	command       string
	destinationID string
}

func newStubStore() *stubStore {
	return &stubStore{
		destinations:  map[string]*domain.PayeeDestination{},
		byFingerprint: map[string]string{},
		events:        map[string][]domain.ChangeEvent{},
		idempotency:   map[string]stubClaim{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func fpKey(partyRef, fingerprint string) string { return partyRef + "|" + fingerprint }

func commandKey(tenantID, idempotencyKey string) string {
	return tenantID + "|" + idempotencyKey
}

func (s *stubStore) recordEvent(destinationID string, tenantID *string, eventType, detail, actor string) {
	s.events[destinationID] = append(s.events[destinationID], domain.ChangeEvent{
		EventID: uuid.New().String(), TenantID: tenantID, DestinationID: destinationID,
		EventType: eventType, Detail: detail, ActorPrincipalID: actor, CreatedAt: time.Now().UTC(),
	})
}

func stubProposalMatches(d *domain.PayeeDestination, req domain.ProposeDestinationRequest) bool {
	scope := req.Scope
	if scope == "" {
		scope = "DEFAULT"
	}
	return d.LegalEntityID == req.LegalEntityID &&
		d.PartyRef == req.PartyRef &&
		d.Scope == scope &&
		d.FinancialInstitution == req.FinancialInstitution &&
		d.AccountIdentifier == req.AccountIdentifier &&
		d.CountryCode == req.CountryCode &&
		d.Currency == req.Currency &&
		d.PayeeName == req.PayeeName &&
		d.SourceType == req.SourceType
}

func (s *stubStore) proposeClaim(tenantID, idempotencyKey string) (stubClaim, bool) {
	c, ok := s.idempotency[commandKey(tenantID, idempotencyKey)]
	return c, ok
}

func (s *stubStore) replayLifecycle(ctx context.Context, idempotencyKey, command, destinationID string) (*domain.PayeeDestination, bool, bool, error) {
	c, ok := s.idempotency[commandKey(middleware.TenantFromContext(ctx), idempotencyKey)]
	if !ok {
		return nil, false, false, nil
	}
	if c.command != command || c.destinationID != destinationID {
		return nil, false, true, domain.ErrIdempotencyKeyConflict
	}
	d, ok := s.destinations[destinationID]
	if !ok {
		return nil, false, true, domain.ErrInvalidTransition
	}
	return d, true, true, nil
}

func (s *stubStore) claimCommand(ctx context.Context, idempotencyKey, command, destinationID string) {
	s.idempotency[commandKey(middleware.TenantFromContext(ctx), idempotencyKey)] = stubClaim{
		command: command, destinationID: destinationID,
	}
}

func (s *stubStore) ProposeDestination(_ context.Context, tenantID string, req domain.ProposeDestinationRequest, principalID, _, idempotencyKey string) (*domain.PayeeDestination, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	if c, found := s.proposeClaim(tenantID, idempotencyKey); found {
		if c.command != domain.CommandProposeDestination {
			return nil, false, domain.ErrIdempotencyKeyConflict
		}
		existing, ok := s.destinations[c.destinationID]
		if !ok {
			return nil, false, domain.ErrIdempotencyKeyConflict
		}
		if !stubProposalMatches(existing, req) {
			return nil, false, domain.ErrIdempotencyKeyConflict
		}
		return existing, true, nil
	}

	fingerprint := domain.Fingerprint(req.FinancialInstitution, req.AccountIdentifier, req.Currency)
	key := fpKey(req.PartyRef, fingerprint)
	if _, taken := s.byFingerprint[key]; taken {
		return nil, false, domain.ErrDuplicateDestination
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
	s.idempotency[commandKey(tenantID, idempotencyKey)] = stubClaim{
		command: domain.CommandProposeDestination, destinationID: d.DestinationID,
	}
	return d, false, nil
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

func (s *stubStore) VerifyDestination(ctx context.Context, destinationID string, req domain.VerifyDestinationRequest, principalID, idempotencyKey string) (*domain.PayeeDestination, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	if d, replay, found, err := s.replayLifecycle(ctx, idempotencyKey, domain.CommandVerifyDestination, destinationID); found {
		return d, replay, err
	}
	d, ok := s.destinations[destinationID]
	if !ok || !domain.CanProposeVerification(d.Status) {
		return nil, false, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	d.Status = domain.StatusVerified
	d.VerificationMethod = req.VerificationMethod
	d.VerificationEvidenceRef = req.VerificationEvidenceRef
	d.VerifiedByPrincipalID = principalID
	d.VerifiedAt = &now
	d.UpdatedAt = now
	s.recordEvent(destinationID, d.TenantID, domain.EventPayeeDestinationVerified, req.VerificationMethod, principalID)
	s.claimCommand(ctx, idempotencyKey, domain.CommandVerifyDestination, destinationID)
	return d, false, nil
}

func (s *stubStore) ApproveDestination(ctx context.Context, destinationID, principalID, _, idempotencyKey string) (*domain.PayeeDestination, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	if d, replay, found, err := s.replayLifecycle(ctx, idempotencyKey, domain.CommandApproveDestination, destinationID); found {
		return d, replay, err
	}
	d, ok := s.destinations[destinationID]
	if !ok || !domain.CanApprove(d.Status) {
		return nil, false, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	d.Status = domain.StatusApprovalPending
	d.ApprovedByPrincipalID = principalID
	d.ApprovedAt = &now
	d.UpdatedAt = now
	s.recordEvent(destinationID, d.TenantID, domain.EventPayeeDestinationApproved, "", principalID)
	s.claimCommand(ctx, idempotencyKey, domain.CommandApproveDestination, destinationID)
	return d, false, nil
}

func (s *stubStore) ActivateDestination(ctx context.Context, destinationID, principalID, _, idempotencyKey string) (*domain.PayeeDestination, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	if d, replay, found, err := s.replayLifecycle(ctx, idempotencyKey, domain.CommandActivateDestination, destinationID); found {
		return d, replay, err
	}
	d, ok := s.destinations[destinationID]
	if !ok || !domain.CanActivate(d.Status) {
		return nil, false, domain.ErrInvalidTransition
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
	s.claimCommand(ctx, idempotencyKey, domain.CommandActivateDestination, destinationID)
	return d, false, nil
}

func (s *stubStore) SuspendDestination(ctx context.Context, destinationID string, req domain.SuspendDestinationRequest, principalID, _, idempotencyKey string) (*domain.PayeeDestination, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	if d, replay, found, err := s.replayLifecycle(ctx, idempotencyKey, domain.CommandSuspendDestination, destinationID); found {
		return d, replay, err
	}
	d, ok := s.destinations[destinationID]
	if !ok || !domain.CanSuspend(d.Status) {
		return nil, false, domain.ErrInvalidTransition
	}
	d.Status = domain.StatusSuspended
	d.SuspendReason = req.Reason
	d.UpdatedAt = time.Now().UTC()
	s.recordEvent(destinationID, d.TenantID, domain.EventPayeeDestinationSuspended, req.Reason, principalID)
	s.claimCommand(ctx, idempotencyKey, domain.CommandSuspendDestination, destinationID)
	return d, false, nil
}

func (s *stubStore) SupersedeDestination(ctx context.Context, destinationID string, req domain.SupersedeDestinationRequest, principalID, _, idempotencyKey string) (*domain.PayeeDestination, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	if d, replay, found, err := s.replayLifecycle(ctx, idempotencyKey, domain.CommandSupersedeDestination, destinationID); found {
		return d, replay, err
	}
	d, ok := s.destinations[destinationID]
	if !ok || !domain.CanSupersede(d.Status) {
		return nil, false, domain.ErrInvalidTransition
	}
	d.Status = domain.StatusSuperseded
	d.UpdatedAt = time.Now().UTC()
	s.recordEvent(destinationID, d.TenantID, domain.EventPayeeDestinationSuperseded, req.Reason, principalID)
	s.claimCommand(ctx, idempotencyKey, domain.CommandSupersedeDestination, destinationID)
	return d, false, nil
}

func (s *stubStore) ListEvents(_ context.Context, destinationID string) ([]domain.ChangeEvent, error) {
	return s.events[destinationID], nil
}
