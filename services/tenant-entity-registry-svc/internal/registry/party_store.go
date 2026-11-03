package registry

import (
	"context"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// PartyStore is the data-access contract for ORG-07 parties (migration 000016). It is separate from Store, like
// OwnershipStore, and the service asserts it: a store without it answers ErrServiceUnavailable rather than
// pretending a party was recorded.
//
// Every method runs in the caller's tenant (domain.TenantFromContext) under row-level security. Version checks
// happen under the party's row lock, so two stewards cannot both win (s19.1: last write wins is prohibited).
type PartyStore interface {
	// CreateParty runs the s10.1 create flow in one transaction under a per-tenant lock: replay check, candidate
	// match, then a safe link, a stewardship case or a new party. It never creates a duplicate. A create_request_id
	// reused for a different request is ErrCreateRequestMismatch.
	CreateParty(ctx context.Context, cmd PartyCreateCommand) (*domain.CreatePartyResult, error)

	// GetPartyView returns the master view in force at asOf as known at knownAt; (nil, nil) when absent.
	GetPartyView(ctx context.Context, partyID string, asOf, knownAt time.Time, reveal bool) (*domain.PartyView, error)

	ProposePartyFact(ctx context.Context, partyID string, req domain.ProposeFactRequest, actor string, allowUnversioned bool) (*PartyFactOutcome, error)
	AddPartyRole(ctx context.Context, partyID string, req domain.AddPartyRoleRequest, actor string, allowUnversioned bool) (*PartyRoleOutcome, error)
	RemovePartyRole(ctx context.Context, partyID, roleID string, req domain.RemovePartyRoleRequest, actor string, allowUnversioned bool) (*PartyRoleOutcome, error)
	LinkPartyIdentifier(ctx context.Context, partyID string, req domain.LinkIdentifierRequest, actor string, allowUnversioned bool) (*PartyIdentifierOutcome, error)

	FindPartiesByIdentifier(ctx context.Context, scheme domain.IdentifierScheme, scope, value string) ([]domain.PartySummary, error)
	SearchParties(ctx context.Context, q domain.PartySearchQuery) ([]domain.PartySummary, string, error)
	GetResolutionCase(ctx context.Context, caseID string) (*domain.ResolutionCase, error)
}

// PartyCreateCommand is a validated create request with its verified context.
type PartyCreateCommand struct {
	Request     domain.CreatePartyRequest
	Actor       string
	Fingerprint string
}

// PartyFactOutcome is what proposing a fact produced. Case is set when the proposal contradicted a verified fact.
type PartyFactOutcome struct {
	Fact  domain.PartyFact       `json:"fact"`
	Party domain.Party           `json:"party"`
	Case  *domain.ResolutionCase `json:"case,omitempty"`
	NoOp  bool                   `json:"-"`
}

// PartyRoleOutcome is what adding or removing a role produced.
type PartyRoleOutcome struct {
	Role  domain.PartyRole `json:"role"`
	Party domain.Party     `json:"party"`
}

// PartyIdentifierOutcome is what linking an identifier produced. Case is set, and Identifier is not, when the
// identifier already names another party: a claim on an identifier somebody holds is a conflict, never a link.
type PartyIdentifierOutcome struct {
	Identifier *domain.PartyIdentifier `json:"identifier,omitempty"`
	Party      domain.Party            `json:"party"`
	Case       *domain.ResolutionCase  `json:"case,omitempty"`
	NoOp       bool                    `json:"-"`
}
