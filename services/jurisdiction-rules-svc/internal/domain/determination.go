package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ZS-JUR-001 s9: jurisdiction determination. "A client-supplied country code may be an
// input fact but never the final authority" (JUR-NEG-05).
//
// What this service can truthfully determine from facts it holds is the set of
// jurisdictions a legal entity is AUTHORISED for, projected from tenant-entity-registry-svc's
// entity.created and entity.jurisdiction.changed events. It does NOT decide place of supply,
// situs or any transaction-level question: no such rules exist yet (Wave 4 Not built), so a
// transaction fact the caller supplies (a claimed jurisdiction) is checked against the
// authorised set and never widens it.
//
//	one authorised jurisdiction for the purpose    -> JURISDICTION_DETERMINED
//	several, and no claim among them               -> AMBIGUOUS_JURISDICTION (review: nothing is guessed)
//	several, and a claim inside the set            -> JURISDICTION_DETERMINED, basis CLAIM_WITHIN_AUTHORISED_SET
//	a claim outside the set                        -> CLAIM_NOT_AUTHORISED (refused, never accepted)
//	none                                           -> NO_APPLICABLE_JURISDICTION (never falls back to the claim)

const (
	OutcomeJurisdictionDetermined = "JURISDICTION_DETERMINED"
	OutcomeJurisdictionAmbiguous  = "AMBIGUOUS_JURISDICTION"
	OutcomeNoApplicable           = "NO_APPLICABLE_JURISDICTION"
	OutcomeClaimNotAuthorised     = "CLAIM_NOT_AUTHORISED"

	BasisSingleAuthorised        = "SINGLE_AUTHORISED_JURISDICTION"
	BasisClaimWithinAuthorised   = "CLAIM_WITHIN_AUTHORISED_SET"
	ClaimNotProvided             = "NOT_PROVIDED"
	ClaimConfirmed               = "CONFIRMED"
	ClaimSelectedAmongAuthorised = "SELECTED_AMONG_AUTHORISED"
	ClaimNotAuthorised           = "NOT_AUTHORISED"
	ClaimNotUsed                 = "NOT_USED"

	PurposeTax    = "TAX"
	PurposeFiling = "FILING"
	PurposeAny    = "ANY"
)

// DeterminationPurposes are the purposes a determination can be asked for.
var DeterminationPurposes = []string{PurposeTax, PurposeFiling, PurposeAny}

// purposeTypes are the assignment types relevant to each purpose. ENTITY_PRIMARY is the entity's
// primary jurisdiction as named when it was created; the producer's own types arrive as assignments.
var purposeTypes = map[string][]string{
	PurposeTax:    {"ENTITY_PRIMARY", "PRIMARY", "SECONDARY", "TAX_ONLY"},
	PurposeFiling: {"ENTITY_PRIMARY", "PRIMARY", "SECONDARY", "FILING_ONLY"},
}

// Assignment is one jurisdiction the entity is assigned to, in force at the decision instant,
// with its code resolved from the jurisdiction registry.
type Assignment struct {
	JurisdictionID      string
	JurisdictionCode    string
	ApplicabilityID     string
	ApplicabilityType   string
	EffectiveFrom       time.Time
	EffectiveTo         *time.Time
	SourceEventID       string
	SourceObjectVersion int64
}

// Candidate is one authorised jurisdiction in a determination.
type Candidate struct {
	JurisdictionID    string     `json:"jurisdiction_id"`
	JurisdictionCode  string     `json:"jurisdiction_code"`
	ApplicabilityType []string   `json:"applicability_types"`
	EffectiveFrom     time.Time  `json:"effective_from"`
	EffectiveTo       *time.Time `json:"effective_to"`
	// Support is the rollout answer (SUPPORTED, NOT_YET_SUPPORTED, SUSPENDED, NO_RELEASED_PACK, NO_ROLLOUT), filled by the caller.
	Support string `json:"support,omitempty"`
	// Evidence names the assignment records the candidate rests on.
	Evidence []EvidenceRef `json:"evidence"`
}

// EvidenceRef points at one consumed assignment.
type EvidenceRef struct {
	ApplicabilityID     string `json:"applicability_id"`
	SourceEventID       string `json:"source_event_id"`
	SourceObjectVersion int64  `json:"source_object_version"`
}

// ClaimCheck reports what happened to the client's claim. It is an input fact: it can select among
// jurisdictions the entity is authorised for and can never add one.
type ClaimCheck struct {
	Claimed string `json:"claimed,omitempty"`
	Status  string `json:"status"`
}

// Determination is the explainable outcome of a jurisdiction determination.
type Determination struct {
	Outcome      string     `json:"outcome"`
	Purpose      string     `json:"purpose"`
	EffectiveAt  time.Time  `json:"effective_at"`
	Basis        string     `json:"basis,omitempty"`
	Jurisdiction *Candidate `json:"jurisdiction,omitempty"`
	Candidates   []Candidate `json:"candidates"`
	Claim        ClaimCheck `json:"claim"`
	Explanation  []string   `json:"explanation"`
}

// ValidPurpose reports whether p is a known determination purpose.
func ValidPurpose(p string) bool { return inList(DeterminationPurposes, p) }

// DetermineJurisdiction decides which jurisdiction applies to an entity for a purpose from its
// authorised assignments. claimed (a jurisdiction code, may be empty) is matched case-insensitively
// and is never an authority.
func DetermineJurisdiction(assignments []Assignment, purpose, claimed string, at time.Time) Determination {
	res := Determination{Purpose: purpose, EffectiveAt: at.UTC(), Candidates: []Candidate{}, Explanation: []string{}}
	step := func(f string, a ...any) { res.Explanation = append(res.Explanation, fmt.Sprintf(f, a...)) }
	claimed = strings.TrimSpace(claimed)
	res.Claim = ClaimCheck{Claimed: claimed, Status: ClaimNotProvided}

	want := purposeTypes[purpose]
	byJur := map[string]*Candidate{}
	var order []string
	for _, a := range assignments {
		if purpose != PurposeAny && !inList(want, a.ApplicabilityType) {
			continue
		}
		c, ok := byJur[a.JurisdictionID]
		if !ok {
			c = &Candidate{JurisdictionID: a.JurisdictionID, JurisdictionCode: a.JurisdictionCode, EffectiveFrom: a.EffectiveFrom, EffectiveTo: a.EffectiveTo, Evidence: []EvidenceRef{}}
			byJur[a.JurisdictionID] = c
			order = append(order, a.JurisdictionID)
		}
		if !inList(c.ApplicabilityType, a.ApplicabilityType) {
			c.ApplicabilityType = append(c.ApplicabilityType, a.ApplicabilityType)
		}
		if a.EffectiveFrom.Before(c.EffectiveFrom) {
			c.EffectiveFrom = a.EffectiveFrom
		}
		c.Evidence = append(c.Evidence, EvidenceRef{ApplicabilityID: a.ApplicabilityID, SourceEventID: a.SourceEventID, SourceObjectVersion: a.SourceObjectVersion})
	}
	for _, id := range order {
		c := byJur[id]
		sort.Strings(c.ApplicabilityType)
		res.Candidates = append(res.Candidates, *c)
	}
	sort.SliceStable(res.Candidates, func(i, j int) bool { return res.Candidates[i].JurisdictionCode < res.Candidates[j].JurisdictionCode })

	n := len(res.Candidates)
	step("the entity is authorised for %d jurisdiction(s) for purpose %s at %s", n, purpose, res.EffectiveAt.Format(time.RFC3339))

	match := -1
	for i, c := range res.Candidates {
		if claimed != "" && strings.EqualFold(c.JurisdictionCode, claimed) {
			match = i
		}
	}

	switch {
	case n == 0:
		res.Outcome = OutcomeNoApplicable
		res.Claim.Status = ClaimNotUsed
		step("no jurisdiction applies: the entity has no assignment for this purpose at this instant. A claimed jurisdiction is never used instead")
	case claimed != "" && match < 0:
		res.Outcome = OutcomeClaimNotAuthorised
		res.Claim.Status = ClaimNotAuthorised
		step("the claimed jurisdiction %q is not one the entity is authorised for: refused, a client claim never widens the authorised set (JUR-NEG-05)", claimed)
	case n == 1:
		c := res.Candidates[0]
		res.Outcome, res.Basis, res.Jurisdiction = OutcomeJurisdictionDetermined, BasisSingleAuthorised, &c
		if claimed != "" {
			res.Claim.Status = ClaimConfirmed
			step("%s is the only authorised jurisdiction and the claim agrees", c.JurisdictionCode)
		} else {
			step("%s is the only authorised jurisdiction", c.JurisdictionCode)
		}
	case match >= 0:
		c := res.Candidates[match]
		res.Outcome, res.Basis, res.Jurisdiction = OutcomeJurisdictionDetermined, BasisClaimWithinAuthorised, &c
		res.Claim.Status = ClaimSelectedAmongAuthorised
		step("the entity is authorised for %d jurisdictions; the claim selects %s from among them. The authority is the entity's assignments, the claim only chooses inside them", n, c.JurisdictionCode)
	default:
		res.Outcome = OutcomeJurisdictionAmbiguous
		res.Claim.Status = ClaimNotUsed
		step("more than one jurisdiction applies and no transaction fact selects between them: this needs review, nothing is guessed")
	}
	return res
}
