package registry

import (
	"context"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// AddressStore is the data-access contract for ORG-08 addresses (migration 000018). Like PartyStore it is asserted
// by the service: a store without it answers ErrServiceUnavailable rather than pretending an address was recorded.
// Every method runs in the caller's tenant under row-level security.
type AddressStore interface {
	// CreateAddress records a new address. A create_request_id replay returns the first result; the same id with
	// different content is ErrCreateRequestMismatch.
	CreateAddress(ctx context.Context, cmd AddressCreateCommand) (*AddressOutcome, error)
	// AmendAddress adds a version under the address row lock; the prior version is superseded, never edited.
	AmendAddress(ctx context.Context, addressID string, req domain.AmendAddressRequest, actor string, allowUnversioned bool) (*AddressOutcome, error)
	// VerifyAddress runs check on the current version under the row lock and records the observation. It never
	// changes the address; it moves the version to VERIFIED only when the observation passed.
	VerifyAddress(ctx context.Context, addressID string, req domain.VerifyAddressRequest, actor string, allowUnversioned bool, check AddressCheck) (*VerifyOutcome, error)
	// GetAddress returns the address with the version in force at asOf; (nil, nil) when absent.
	GetAddress(ctx context.Context, addressID string, asOf time.Time, history bool) (*domain.AddressView, error)
	SearchAddresses(ctx context.Context, q domain.AddressSearchQuery) ([]domain.AddressSummary, string, error)
	// LinkAddressToParty uses an address for a party in its own capacity. A new REGISTERED link ends the party's
	// previous one on the new start date; both rows stay.
	LinkAddressToParty(ctx context.Context, partyID string, req domain.LinkAddressRequest, actor string, allowUnversioned bool) (*AddressLinkOutcome, error)
	ListPartyAddresses(ctx context.Context, partyID string, asOf time.Time, includeEnded bool) ([]domain.PartyAddress, error)
}

// AddressCheck observes a version. It is run by the store under the address lock.
type AddressCheck func(v domain.AddressVersion) domain.AddressObservation

// AddressCreateCommand is a validated create with its verified context.
type AddressCreateCommand struct {
	Request     domain.CreateAddressRequest
	Actor       string
	Fingerprint string
}

// AddressOutcome is an address after a create or amend.
type AddressOutcome struct {
	Address  domain.Address        `json:"address"`
	Version  domain.AddressVersion `json:"version"`
	Replayed bool                  `json:"-"`
	NoOp     bool                  `json:"-"`
}

// VerifyOutcome is the observation and the address it was made on.
type VerifyOutcome struct {
	Address     domain.Address            `json:"address"`
	Version     domain.AddressVersion     `json:"version"`
	Observation domain.AddressObservation `json:"observation"`
}

// AddressLinkOutcome is a link and the party it was made for.
type AddressLinkOutcome struct {
	Link  domain.AddressLink  `json:"link"`
	Party domain.Party        `json:"party"`
	Ended *domain.AddressLink `json:"ended,omitempty"`
	NoOp  bool                `json:"-"`
}
