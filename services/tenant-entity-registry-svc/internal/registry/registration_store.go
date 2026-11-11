package registry

import (
	"context"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// RegistrationStore is the data-access contract for ORG-09 registrations (migration 000020). Asserted by the
// service like PartyStore and AddressStore. Every method except SweepRegistrationExpiries runs in the caller's
// tenant under row-level security. A store returns numbers as recorded; masking is the service's job.
type RegistrationStore interface {
	// CreateRegistration records a DRAFT registration. A create_request_id replay returns the first result; the same
	// id with different content is ErrCreateRequestMismatch. A live registration with the same authority, type,
	// jurisdiction and number is ErrConflict.
	CreateRegistration(ctx context.Context, cmd RegistrationCreateCommand) (*RegistrationOutcome, error)
	// VerifyRegistration moves DRAFT or SUSPENDED to ACTIVE on evidence. A material registration is not verified by
	// the principal who created it (ErrSelfApproval).
	VerifyRegistration(ctx context.Context, id string, req domain.VerifyRegistrationRequest, actor string, allowUnversioned bool) (*RegistrationOutcome, error)
	// RenewRegistration extends the expiry on evidence. A material registration is not renewed by the principal who
	// last made it valid (ErrSelfApproval).
	RenewRegistration(ctx context.Context, id string, req domain.RenewRegistrationRequest, actor string, allowUnversioned bool) (*RegistrationOutcome, error)
	// ChangeRegistrationStatus is SuspendRegistration, ExpireRegistration or RevokeRegistration, named by command.
	ChangeRegistrationStatus(ctx context.Context, id, command string, to domain.RegistrationStatus, req domain.RegistrationEndRequest, actor string, allowUnversioned bool) (*RegistrationOutcome, error)

	// GetRegistration returns a registration; (nil, nil) when absent.
	GetRegistration(ctx context.Context, id string) (*domain.Registration, error)
	// RegistrationHolder returns the legal entity or party holding a registration, for authorization.
	RegistrationHolder(ctx context.Context, id string) (legalEntityID, partyID string, err error)
	RegistrationHistory(ctx context.Context, id string) ([]domain.RegistrationTransition, error)
	// FindRegistrationsByNumber matches on the normalized number, optionally narrowed by type, authority and jurisdiction.
	FindRegistrationsByNumber(ctx context.Context, number string, typ domain.RegistrationType, authority, jurisdiction string) ([]domain.Registration, error)
	ListRegistrations(ctx context.Context, q domain.RegistrationSearchQuery) ([]domain.Registration, string, error)
	// ListExpiringRegistrations lists ACTIVE and SUSPENDED registrations whose expiry falls on or before asOf plus
	// withinDays, soonest first, including any already past it that the sweep has not recorded.
	ListExpiringRegistrations(ctx context.Context, withinDays int, asOf time.Time) ([]domain.Registration, error)

	// SweepRegistrationExpiries records the expiry of registrations past their date and warns once about those
	// within warnDays, across tenants. It is the only cross-tenant method; each registration is changed inside its
	// own tenant's policy, in one transaction with its history row and event.
	SweepRegistrationExpiries(ctx context.Context, now time.Time, warnDays int) (SweepResult, error)
}

// RegistrationCreateCommand is a validated create with its verified context.
type RegistrationCreateCommand struct {
	Request     domain.CreateRegistrationRequest
	Actor       string
	Fingerprint string
}

// RegistrationOutcome is a registration after a command, and the step it recorded.
type RegistrationOutcome struct {
	Registration domain.Registration           `json:"registration"`
	Transition   domain.RegistrationTransition `json:"transition"`
	// Superseded is the registration this one replaced when it was verified.
	Superseded *domain.Registration `json:"superseded,omitempty"`
	Replayed   bool                 `json:"-"`
}

// SweepResult is what one expiry sweep did.
type SweepResult struct {
	Expired int `json:"expired"`
	Warned  int `json:"warned"`
}
