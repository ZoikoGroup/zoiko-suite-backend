package registry

// ORG-09 Registration & License: CreateRegistration, VerifyRegistration, RenewRegistration, SuspendRegistration,
// ExpireRegistration, RevokeRegistration and the reads GetRegistration, FindByNumber, ListRegistrations,
// ListExpiringRegistrations and GetRegistrationHistory (ZS-SVC-I-001 s4.9).
//
// A command is authorized against its holder: the legal entity when a legal entity holds the registration, the tenant
// when a party does. Sensitive numbers (tax references, payroll registrations) are masked in every response unless
// the caller holds the reveal permission and asks. In-force and lapsed are computed from the dates on every read, so a
// registration past its expiry never reads as active even before the sweep has recorded it. None of this decides a tax
// outcome: a TAX_REFERENCE registration is evidence, and TAX-01 stays authoritative for what it means.

import (
	"context"
	"fmt"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *Service) registrationStore() (RegistrationStore, error) {
	rs, ok := s.store.(RegistrationStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return rs, nil
}

// authorizeHolder authorizes an action at the holder named, or at the tenant for a party holder.
func (s *Service) authorizeHolder(ctx context.Context, legalEntityID, partyID, resource, action string) error {
	if legalEntityID != "" {
		return s.authorizeEntity(ctx, legalEntityID, resource, action)
	}
	_ = partyID
	return s.authorize(ctx, resource, action)
}

// finishRegistration evaluates the dates and masks a sensitive number the caller may not see.
func finishRegistration(r *domain.Registration, reveal bool, now time.Time) {
	r.Evaluate(now)
	if r.RegistrationType.Sensitive() && !reveal {
		r.Number, r.Masked = domain.MaskNumber(r.Number), true
	}
}

func finishOutcome(o *RegistrationOutcome, now time.Time) {
	finishRegistration(&o.Registration, false, now)
	if o.Superseded != nil {
		finishRegistration(o.Superseded, false, now)
	}
}

// CreateRegistration is CreateRegistration.
func (s *Service) CreateRegistration(ctx context.Context, req domain.CreateRegistrationRequest) (*RegistrationOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorizeHolder(ctx, req.HolderLegalEntityID, req.HolderPartyID, "registration", "create"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	if req.HolderLegalEntityID != "" {
		if err := s.assertEntityOperational(ctx, req.HolderLegalEntityID); err != nil {
			return nil, err
		}
	}
	rs, err := s.registrationStore()
	if err != nil {
		return nil, err
	}
	out, err := rs.CreateRegistration(ctx, RegistrationCreateCommand{Request: req, Actor: domain.PrincipalFromContext(ctx), Fingerprint: req.Fingerprint()})
	if err != nil {
		return nil, fmt.Errorf("store.CreateRegistration: %w", err)
	}
	finishOutcome(out, time.Now().UTC())
	return out, nil
}

// command runs one named command on an existing registration: authorize at its holder, then the store.
func (s *Service) registrationCommand(ctx context.Context, id, action string, run func(rs RegistrationStore, actor string) (*RegistrationOutcome, error)) (*RegistrationOutcome, error) {
	rs, err := s.registrationStore()
	if err != nil {
		return nil, err
	}
	entityID, partyID, err := rs.RegistrationHolder(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store.RegistrationHolder: %w", err)
	}
	if err := s.authorizeHolder(ctx, entityID, partyID, "registration", action); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	out, err := run(rs, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.%s: %w", action, err)
	}
	finishOutcome(out, time.Now().UTC())
	return out, nil
}

// VerifyRegistration is VerifyRegistration.
func (s *Service) VerifyRegistration(ctx context.Context, id string, req domain.VerifyRegistrationRequest) (*RegistrationOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return s.registrationCommand(ctx, id, "verify", func(rs RegistrationStore, actor string) (*RegistrationOutcome, error) {
		return rs.VerifyRegistration(ctx, id, req, actor, s.expectedVersionOptional)
	})
}

// RenewRegistration is RenewRegistration.
func (s *Service) RenewRegistration(ctx context.Context, id string, req domain.RenewRegistrationRequest) (*RegistrationOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return s.registrationCommand(ctx, id, "renew", func(rs RegistrationStore, actor string) (*RegistrationOutcome, error) {
		return rs.RenewRegistration(ctx, id, req, actor, s.expectedVersionOptional)
	})
}

// ChangeRegistrationStatus is SuspendRegistration, ExpireRegistration or RevokeRegistration: to is SUSPENDED,
// EXPIRED or REVOKED.
func (s *Service) ChangeRegistrationStatus(ctx context.Context, id string, to domain.RegistrationStatus, req domain.RegistrationEndRequest) (*RegistrationOutcome, error) {
	command, action := "", ""
	switch to {
	case domain.RegSuspended:
		command, action = "SuspendRegistration", "suspend"
	case domain.RegExpired:
		command, action = "ExpireRegistration", "expire"
	case domain.RegRevoked:
		command, action = "RevokeRegistration", "revoke"
	default:
		return nil, fmt.Errorf("%w: a registration is suspended, expired or revoked by command", ErrInvalidInput)
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return s.registrationCommand(ctx, id, action, func(rs RegistrationStore, actor string) (*RegistrationOutcome, error) {
		return rs.ChangeRegistrationStatus(ctx, id, command, to, req, actor, s.expectedVersionOptional)
	})
}

// GetRegistration is GetRegistration. Revealing a sensitive number is its own permission and is refused, not
// silently masked, when asked for without it.
func (s *Service) GetRegistration(ctx context.Context, id string, reveal bool) (*domain.Registration, error) {
	if reveal {
		if err := s.authorize(ctx, "registration.number", "reveal"); err != nil {
			return nil, err
		}
	}
	rs, err := s.registrationStore()
	if err != nil {
		return nil, err
	}
	r, err := rs.GetRegistration(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store.GetRegistration: %w", err)
	}
	if r == nil {
		return nil, ErrNotFound
	}
	finishRegistration(r, reveal, time.Now().UTC())
	return r, nil
}

// GetRegistrationHistory is GetRegistrationHistory: every recorded step, in order.
func (s *Service) GetRegistrationHistory(ctx context.Context, id string) ([]domain.RegistrationTransition, error) {
	rs, err := s.registrationStore()
	if err != nil {
		return nil, err
	}
	out, err := rs.RegistrationHistory(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store.RegistrationHistory: %w", err)
	}
	return out, nil
}

func (s *Service) finishAll(rs []domain.Registration, reveal bool) []domain.Registration {
	now := time.Now().UTC()
	for i := range rs {
		finishRegistration(&rs[i], reveal, now)
	}
	return rs
}

// FindRegistrationsByNumber is FindByNumber. The number is matched on its normalized form; results are masked
// unless revealed.
func (s *Service) FindRegistrationsByNumber(ctx context.Context, number string, typ domain.RegistrationType, authority, jurisdiction string, reveal bool) ([]domain.Registration, error) {
	if domain.NormalizeRegistrationNumber(number) == "" {
		return nil, fmt.Errorf("%w: number is required", ErrInvalidInput)
	}
	if typ != "" && !typ.Valid() {
		return nil, fmt.Errorf("%w: type is not a registration type", ErrInvalidInput)
	}
	if reveal {
		if err := s.authorize(ctx, "registration.number", "reveal"); err != nil {
			return nil, err
		}
	}
	rs, err := s.registrationStore()
	if err != nil {
		return nil, err
	}
	out, err := rs.FindRegistrationsByNumber(ctx, number, typ, authority, jurisdiction)
	if err != nil {
		return nil, fmt.Errorf("store.FindRegistrationsByNumber: %w", err)
	}
	return s.finishAll(out, reveal), nil
}

// ListRegistrations lists registrations by holder, type, status or jurisdiction.
func (s *Service) ListRegistrations(ctx context.Context, q domain.RegistrationSearchQuery, reveal bool) ([]domain.Registration, string, error) {
	if q.Type != "" && !q.Type.Valid() {
		return nil, "", fmt.Errorf("%w: type is not a registration type", ErrInvalidInput)
	}
	if q.Status != "" && !q.Status.Valid() {
		return nil, "", fmt.Errorf("%w: status is not a registration status", ErrInvalidInput)
	}
	if reveal {
		if err := s.authorize(ctx, "registration.number", "reveal"); err != nil {
			return nil, "", err
		}
	}
	rs, err := s.registrationStore()
	if err != nil {
		return nil, "", err
	}
	out, next, err := rs.ListRegistrations(ctx, q)
	if err != nil {
		return nil, "", fmt.Errorf("store.ListRegistrations: %w", err)
	}
	return s.finishAll(out, reveal), next, nil
}

// ListExpiringRegistrations is ListExpiringRegistrations: expiring within withinDays of asOf, and any already
// lapsed, so expiry is surfaced whether or not the sweep has run.
func (s *Service) ListExpiringRegistrations(ctx context.Context, withinDays int, asOf time.Time, reveal bool) ([]domain.Registration, error) {
	if withinDays < 0 || withinDays > 3650 {
		return nil, fmt.Errorf("%w: within_days must be from 0 to 3650", ErrInvalidInput)
	}
	if reveal {
		if err := s.authorize(ctx, "registration.number", "reveal"); err != nil {
			return nil, err
		}
	}
	rs, err := s.registrationStore()
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	out, err := rs.ListExpiringRegistrations(ctx, withinDays, asOf)
	if err != nil {
		return nil, fmt.Errorf("store.ListExpiringRegistrations: %w", err)
	}
	return s.finishAll(out, reveal), nil
}

// SweepRegistrationExpiries records expiries and warns about those coming, across tenants. It is the system's own
// act, not a caller's, so it takes no principal; the store attributes it to system:expiry-sweep.
func (s *Service) SweepRegistrationExpiries(ctx context.Context, now time.Time, warnDays int) (SweepResult, error) {
	rs, err := s.registrationStore()
	if err != nil {
		return SweepResult{}, err
	}
	res, err := rs.SweepRegistrationExpiries(ctx, now, warnDays)
	if err != nil {
		return res, fmt.Errorf("store.SweepRegistrationExpiries: %w", err)
	}
	return res, nil
}
