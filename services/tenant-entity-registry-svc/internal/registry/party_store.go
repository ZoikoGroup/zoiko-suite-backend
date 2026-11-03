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
	// SkipRequestRecord is set when a steward creates the party a held request asked for: the request id already
	// has its REVIEW_REQUIRED record, and a second would break the append-only trail.
	SkipRequestRecord bool
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

// PartyResolutionStore is the MDM-05 half: matching, stewardship decisions, merge, split and merge history. Like
// PartyStore it is asserted by the service.
type PartyResolutionStore interface {
	// MatchParties answers what a create would do for this request, and why, without writing anything.
	MatchParties(ctx context.Context, req domain.MatchPartiesRequest) (*domain.MatchPartiesResult, error)
	ListResolutionCases(ctx context.Context, status, caseType string, limit int, after string) ([]domain.ResolutionCase, string, error)
	// DecideResolutionCase settles a case under its row lock. The decider must differ from the opener
	// (ErrSelfApproval); a stale case version is ErrVersionConflict.
	DecideResolutionCase(ctx context.Context, caseID string, req domain.CaseDecisionRequest, actor string, allowUnversioned bool) (*CaseDecisionOutcome, error)
	// MergeParties is MDM-05 s18.2 in one transaction. See MergeOutcome.
	MergeParties(ctx context.Context, caseID string, req domain.MergePartiesRequest, actor string) (*MergeOutcome, error)
	// SplitParties corrects a merge with new lineage; it erases nothing.
	SplitParties(ctx context.Context, caseID string, req domain.SplitPartiesRequest, actor string) (*SplitOutcome, error)
	ExplainMergeHistory(ctx context.Context, partyID string) (*domain.MergeHistory, error)
}

// CaseDecisionOutcome is a decided case, and the party the decision made or named.
type CaseDecisionOutcome struct {
	Case  domain.ResolutionCase `json:"case"`
	Party *domain.Party         `json:"party,omitempty"`
}

// MergeOutcome is a committed merge: the survivor, the aliases (each still resolvable, status MERGED), the lineage
// rows and what moved.
type MergeOutcome struct {
	Survivor domain.Party          `json:"survivor"`
	Aliases  []domain.Party        `json:"aliases"`
	Lineage  []domain.LineageEvent `json:"lineage"`
	Case     domain.ResolutionCase `json:"case"`
}

// SplitOutcome is a committed split: the party that is distinct again, and its former survivor.
type SplitOutcome struct {
	Restored domain.Party        `json:"restored"`
	Survivor domain.Party        `json:"survivor"`
	Lineage  domain.LineageEvent `json:"lineage"`
}
