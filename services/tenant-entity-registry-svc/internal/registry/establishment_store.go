package registry

import (
	"context"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// EstablishmentStore is the data-access contract for ORG-08 establishments and the entity address links (migration
// 000019). Asserted by the service like PartyStore and AddressStore.
type EstablishmentStore interface {
	// CreateEstablishment records a PROPOSED establishment. A create_request_id replay returns the first result;
	// the same id with different content is ErrCreateRequestMismatch.
	CreateEstablishment(ctx context.Context, cmd EstablishmentCreateCommand) (*EstablishmentOutcome, error)
	// ChangeEstablishmentStatus moves an establishment under its row lock, recording the transition. A material
	// establishment is activated by a principal other than its creator (ErrSelfApproval).
	ChangeEstablishmentStatus(ctx context.Context, establishmentID string, req domain.ChangeEstablishmentStatusRequest, actor string, allowUnversioned bool) (*EstablishmentOutcome, error)
	// GetEstablishment returns the establishment with the address version in force at asOf; (nil, nil) when absent.
	GetEstablishment(ctx context.Context, establishmentID string, asOf time.Time, history bool) (*domain.EstablishmentView, error)
	// EstablishmentEntity returns the legal entity an establishment belongs to; ErrNotFound when absent.
	EstablishmentEntity(ctx context.Context, establishmentID string) (string, error)
	// ListEntityEstablishments lists an entity's establishments by current status ("" for all).
	ListEntityEstablishments(ctx context.Context, legalEntityID string, status domain.EstablishmentStatus) ([]domain.Establishment, error)
	// EstablishmentsAsOf lists the establishments that were ACTIVE on the date, reconstructed from the recorded
	// transitions, not from the current status.
	EstablishmentsAsOf(ctx context.Context, legalEntityID string, asOf time.Time) ([]domain.EstablishmentView, error)
	// ListEntityAddresses lists the addresses an entity has or had; the registered office is one.
	ListEntityAddresses(ctx context.Context, legalEntityID string, asOf time.Time, includeEnded bool) ([]domain.EntityAddress, error)
}

// EstablishmentCreateCommand is a validated create with its verified context.
type EstablishmentCreateCommand struct {
	Request     domain.CreateEstablishmentRequest
	Actor       string
	Fingerprint string
}

// EstablishmentOutcome is an establishment after a create or a status change.
type EstablishmentOutcome struct {
	Establishment domain.Establishment           `json:"establishment"`
	Transition    domain.EstablishmentTransition `json:"transition"`
	Replayed      bool                           `json:"-"`
}
