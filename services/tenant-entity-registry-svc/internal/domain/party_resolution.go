package domain

// MDM-05 matching, merge, split and stewardship decisions (ZS-SVC-U-001 s18.2, s19; ZS-SVC-I-001 s4.7
// MergeParties, SplitParty, ExplainMergeHistory).

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PartyMerged is the status of a party that was merged into a survivor. Its id still resolves.
const PartyMerged PartyStatus = "MERGED"

// Decisions a steward can make on a case.
const (
	DecisionMerge             = "MERGE"
	DecisionCreateNew         = "CREATE_NEW"
	DecisionLinkExisting      = "LINK_EXISTING"
	DecisionAcceptProposed    = "ACCEPT_PROPOSED_FACT"
	DecisionKeepVerified      = "KEEP_VERIFIED"
	DecisionKeepHolder        = "KEEP_HOLDER"
	DecisionMoveToRequester   = "MOVE_TO_REQUESTER"
	DecisionDismiss           = "DISMISS"
	CaseResolved              = "RESOLVED"
	CaseDismissed             = "DISMISSED"
	LineageMerge              = "MERGE"
	LineageSplit              = "SPLIT"
	MaxMergedPartiesPerDecide = 10
)

// DecisionsFor is what a steward may decide for each kind of case. MERGE is its own command, not a decision here.
func DecisionsFor(caseType string) []string {
	switch caseType {
	case CaseDuplicateReview:
		return []string{DecisionCreateNew, DecisionLinkExisting, DecisionDismiss}
	case CaseFactConflict:
		return []string{DecisionAcceptProposed, DecisionKeepVerified}
	case CaseIdentifierConflict:
		return []string{DecisionKeepHolder, DecisionMoveToRequester}
	}
	return nil
}

// MatchPartiesRequest is POST /v1/resolution:match: what a create would be matched against, without creating.
type MatchPartiesRequest struct {
	PartyKind     PartyKind              `json:"party_kind"`
	Names         []PartyNameInput       `json:"names"`
	Country       string                 `json:"country,omitempty"`
	Identifiers   []PartyIdentifierInput `json:"identifiers,omitempty"`
	LegalEntityID string                 `json:"legal_entity_id,omitempty"`
}

// Validate checks the request without touching state.
func (r *MatchPartiesRequest) Validate() error {
	if !r.PartyKind.Valid() {
		return fmt.Errorf("party_kind must be LEGAL_ENTITY, ORGANIZATION_NON_LEGAL, NATURAL_PERSON or PUBLIC_AUTHORITY")
	}
	if len(r.Names) == 0 && len(r.Identifiers) == 0 {
		return fmt.Errorf("a match needs at least a name or an identifier")
	}
	if len(r.Names) > MaxPartyNames || len(r.Identifiers) > MaxPartyIdentifiers {
		return fmt.Errorf("too many names or identifiers")
	}
	for i, n := range r.Names {
		if !n.Type.IsName() || strings.TrimSpace(n.Value) == "" {
			return fmt.Errorf("names[%d] needs a name type and a value", i)
		}
	}
	for i := range r.Identifiers {
		if err := r.Identifiers[i].Validate(); err != nil {
			return fmt.Errorf("identifiers[%d]: %w", i, err)
		}
	}
	if r.Country != "" && !validCountry(r.Country) {
		return fmt.Errorf("country must be an ISO 3166-1 alpha-2 code")
	}
	return nil
}

// AsCreate is the request as the matcher reads a create. It carries no source or evidence: nothing is written.
func (r *MatchPartiesRequest) AsCreate() CreatePartyRequest {
	return CreatePartyRequest{PartyKind: r.PartyKind, Names: r.Names, Country: r.Country, Identifiers: r.Identifiers,
		LegalEntityID: r.LegalEntityID}
}

// MatchPartiesResult is the explainable, tenant-scoped answer: what a create would do, and why.
type MatchPartiesResult struct {
	// Verdict is SAFE_LINK (a tier A match: a create would return that party), REVIEW_REQUIRED (a create would
	// open a case) or NO_MATCH (a create would make a new party).
	Verdict    string           `json:"verdict"`
	CaseType   string           `json:"case_type,omitempty"`
	Candidates []MatchCandidate `json:"candidates"`
}

// MergePartiesRequest is POST /v1/resolution/{case}:merge.
type MergePartiesRequest struct {
	SurvivorPartyID string   `json:"survivor_party_id"`
	MergedPartyIDs  []string `json:"merged_party_ids"`
	// ExpectedMasterVersions holds the version of every party involved, survivor included: the decision is made on
	// what the steward saw, and a party that moved since is 409 VERSION_CONFLICT (s18.2 step 2).
	ExpectedMasterVersions map[string]int64 `json:"expected_master_versions"`
	Reason                 string           `json:"reason"`
	EvidenceRef            string           `json:"evidence_ref"`
	CorrelationID          string           `json:"correlation_id,omitempty"`
}

// Validate checks the request without touching state.
func (r *MergePartiesRequest) Validate() error {
	if _, err := uuid.Parse(r.SurvivorPartyID); err != nil {
		return fmt.Errorf("survivor_party_id must be a party id")
	}
	if len(r.MergedPartyIDs) == 0 || len(r.MergedPartyIDs) > MaxMergedPartiesPerDecide {
		return fmt.Errorf("merged_party_ids holds between 1 and %d parties", MaxMergedPartiesPerDecide)
	}
	seen := map[string]bool{r.SurvivorPartyID: true}
	for _, id := range r.MergedPartyIDs {
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("merged_party_ids holds %q, which is not a party id", id)
		}
		if seen[id] {
			return fmt.Errorf("a party is merged once, and never into itself")
		}
		seen[id] = true
	}
	for id := range seen {
		if r.ExpectedMasterVersions[id] < 1 {
			return fmt.Errorf("expected_master_versions needs the version of party %s", id)
		}
	}
	if strings.TrimSpace(r.Reason) == "" || strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("reason and evidence_ref are required: a merge is decided on evidence")
	}
	return nil
}

// SplitPartiesRequest is POST /v1/resolution/{case}:split: the merge was wrong, this party is distinct.
type SplitPartiesRequest struct {
	MergedPartyID          string           `json:"merged_party_id"`
	ExpectedMasterVersions map[string]int64 `json:"expected_master_versions"`
	Reason                 string           `json:"reason"`
	EvidenceRef            string           `json:"evidence_ref"`
	CorrelationID          string           `json:"correlation_id,omitempty"`
}

// Validate checks the request without touching state. ExpectedMasterVersions needs the merged party and its
// survivor, which the store names when it reads the lineage.
func (r *SplitPartiesRequest) Validate() error {
	if _, err := uuid.Parse(r.MergedPartyID); err != nil {
		return fmt.Errorf("merged_party_id must be a party id")
	}
	if r.ExpectedMasterVersions[r.MergedPartyID] < 1 {
		return fmt.Errorf("expected_master_versions needs the version of the merged party")
	}
	if strings.TrimSpace(r.Reason) == "" || strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("reason and evidence_ref are required: a split is decided on evidence")
	}
	return nil
}

// CaseDecisionRequest is POST /v1/resolution-cases/{id}/decision.
type CaseDecisionRequest struct {
	Decision string `json:"decision"`
	// PartyID names the party for LINK_EXISTING.
	PartyID             string `json:"party_id,omitempty"`
	Note                string `json:"note"`
	ExpectedCaseVersion int64  `json:"expected_case_version"`
	CorrelationID       string `json:"correlation_id,omitempty"`
}

// Validate checks the request without touching state; the case type decides which decisions are open.
func (r *CaseDecisionRequest) Validate() error {
	if r.Decision == "" {
		return fmt.Errorf("decision is required")
	}
	if strings.TrimSpace(r.Note) == "" {
		return fmt.Errorf("note is required: a decision states why")
	}
	if r.Decision == DecisionLinkExisting {
		if _, err := uuid.Parse(r.PartyID); err != nil {
			return fmt.Errorf("LINK_EXISTING needs party_id")
		}
	}
	return nil
}

// LineageEvent is one row of merge or split history.
type LineageEvent struct {
	LineageID     string         `json:"lineage_id"`
	EventType     string         `json:"event_type"`
	CaseID        string         `json:"case_id"`
	SurvivorID    string         `json:"survivor_id"`
	PartyID       string         `json:"party_id"`
	Actor         string         `json:"actor"`
	IndependentOf string         `json:"independent_of"`
	Reason        string         `json:"reason"`
	EvidenceRef   string         `json:"evidence_ref"`
	OccurredAt    time.Time      `json:"occurred_at"`
	Details       map[string]any `json:"details"`
}

// MergeHistory is ExplainMergeHistory: where a party's id resolves, what was merged into it, and every merge and
// split that touched it, oldest first.
type MergeHistory struct {
	PartyID    string         `json:"party_id"`
	Status     PartyStatus    `json:"status"`
	ResolvesTo string         `json:"resolves_to"`
	AliasIDs   []string       `json:"alias_ids"`
	Lineage    []LineageEvent `json:"lineage"`
}
