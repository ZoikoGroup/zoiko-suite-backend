package registry

import (
	"context"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// ContactPointStore is the data-access contract for MDM-01 party contact points (migration 000028). Asserted by the
// service like PartyStore and EstablishmentStore.
type ContactPointStore interface {
	// CreateContactPoint records a PROPOSED contact point for a party of the caller's tenant. ErrNotFound for a party
	// of another tenant or none; ErrStateConflict for an INACTIVE or merged party; ErrConflict for a live duplicate.
	CreateContactPoint(ctx context.Context, cmd ContactPointCreateCommand) (*ContactPointOutcome, error)
	// VerifyContactPoint moves PROPOSED to VERIFIED under the row lock. The principal who proposed it is refused
	// (ErrSelfApproval).
	VerifyContactPoint(ctx context.Context, id string, req domain.VerifyContactPointRequest, actor string, allowUnversioned bool) (*ContactPointOutcome, error)
	// RetireContactPoint ends a PROPOSED or VERIFIED contact point, finally.
	RetireContactPoint(ctx context.Context, id string, req domain.RetireContactPointRequest, actor string, allowUnversioned bool) (*ContactPointOutcome, error)
	// ListPartyContactPoints lists a party's contact points, values included. ErrNotFound for an unknown party.
	ListPartyContactPoints(ctx context.Context, partyID string, q domain.ContactPointQuery) ([]domain.ContactPoint, error)
}

// ContactPointCreateCommand is a validated, normalised create with its verified context.
type ContactPointCreateCommand struct {
	PartyID string
	Request domain.CreateContactPointRequest
	Actor   string
}

// ContactPointOutcome is a contact point after a command. The address value is never in it.
type ContactPointOutcome struct {
	ContactPoint domain.ContactPoint `json:"contact_point"`
}
