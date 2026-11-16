package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ZS-JUR-001 s9 and s25 (JUR-1): the entity facts a jurisdiction decision rests on beyond the
// jurisdiction assignments: establishments (ORG-6) and registrations (ORG-7), projected from
// tenant-entity-registry-svc events, plus the facts the transaction supplies.
//
// Rules, in order. None of them adds a jurisdiction the entity is not authorised for, and none guesses:
//
//	a named establishment or registration must exist for the entity and be in force at the instant,
//	and its jurisdiction must be one the entity is authorised for         -> otherwise FACT_NOT_USABLE (422, names the fact)
//	every selecting fact (claim, establishment, registration) must agree  -> otherwise FACTS_CONFLICT (409)
//	one agreed jurisdiction selects among the authorised ones             -> JURISDICTION_DETERMINED
//	several authorised and nothing selects                                -> INSUFFICIENT_FACTS (422, names what would select)
//
// Counterparty and transaction facts are recorded as inputs and flagged as not deciding: the place-of-supply
// rules they would feed do not exist yet.

const (
	FactEstablishment = "ESTABLISHMENT"
	FactRegistration  = "REGISTRATION"

	OutcomeInsufficientFacts = "INSUFFICIENT_FACTS"
	OutcomeFactNotUsable     = "FACT_NOT_USABLE"
	OutcomeFactsConflict     = "FACTS_CONFLICT"

	BasisFactsWithinAuthorised = "FACTS_WITHIN_AUTHORISED_SET"

	// Fact names a caller can supply, as they appear in missing_facts.
	FactNameEstablishmentID = "establishment_id"
	FactNameRegistrationID  = "registration_id"
	FactNameClaimed         = "claimed_jurisdiction"
)

// SupplyTypes and BusinessStatuses are the accepted transaction and counterparty classifications.
var (
	SupplyTypes      = []string{"GOODS", "SERVICES"}
	BusinessStatuses = []string{"B2B", "B2C"}
)

// FactRow is one projected status event of an establishment or registration.
type FactRow struct {
	EventID       string
	FactKind      string
	FactID        string
	Subtype       string
	Jurisdiction  string // registration: its jurisdiction code
	AddressID     string // establishment: its address
	AddressCtry   string // establishment: the address country, empty when not known
	Status        string
	EffectiveDate string // YYYY-MM-DD
	ExpiryDate    string // YYYY-MM-DD, may be empty
	ObjectVersion int64
}

// FactView is a fact as it stood on the decision date.
type FactView struct {
	Kind          string
	ID            string
	Subtype       string
	Status        string
	Code          string // the jurisdiction code the fact points at, empty when not known
	InForce       bool
	Reason        string // why it is not in force
	SourceEventID string
	Version       int64
}

// CurrentFacts reduces status events to the state of each fact on atDate (YYYY-MM-DD): the latest event
// effective on or before that date wins, so a later deactivation or expiry is not read back into the past.
func CurrentFacts(rows []FactRow, atDate string) []FactView {
	best := map[string]FactRow{}
	for _, r := range rows {
		if r.EffectiveDate > atDate {
			continue
		}
		k := r.FactKind + "\x00" + r.FactID
		cur, ok := best[k]
		if !ok || r.EffectiveDate > cur.EffectiveDate || (r.EffectiveDate == cur.EffectiveDate && r.ObjectVersion > cur.ObjectVersion) {
			best[k] = r
		}
	}
	out := make([]FactView, 0, len(best))
	for _, r := range best {
		v := FactView{Kind: r.FactKind, ID: r.FactID, Subtype: r.Subtype, Status: r.Status, SourceEventID: r.EventID, Version: r.ObjectVersion}
		switch r.FactKind {
		case FactEstablishment:
			v.Code = strings.ToUpper(strings.TrimSpace(r.AddressCtry))
			v.InForce = r.Status == "ACTIVE"
			if !v.InForce {
				v.Reason = "status is " + r.Status
			}
		case FactRegistration:
			v.Code = strings.ToUpper(strings.TrimSpace(r.Jurisdiction))
			switch {
			case r.Status != "ACTIVE":
				v.Reason = "status is " + r.Status
			case r.ExpiryDate != "" && r.ExpiryDate < atDate:
				v.Reason = "expired on " + r.ExpiryDate
			default:
				v.InForce = true
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Counterparty and Transaction are facts the request supplies. They are recorded, never authoritative.
type Counterparty struct {
	Country        string `json:"country,omitempty"`
	BusinessStatus string `json:"business_status,omitempty"`
}

type Transaction struct {
	SupplyType string `json:"supply_type,omitempty"`
}

// FactInputs is what the caller names beyond the claim.
type FactInputs struct {
	EstablishmentID string
	RegistrationID  string
	Counterparty    *Counterparty
	Transaction     *Transaction
}

// MissingFact names a fact the decision needs, or one that cannot be used, and why.
type MissingFact struct {
	Fact   string `json:"fact"`
	Reason string `json:"reason"`
}

// FactEvidence is one consumed establishment or registration. RefDigest stands in for the id: the decision
// ledger is platform-wide, so it holds no tenant object ids.
type FactEvidence struct {
	Kind                string `json:"kind"`
	RefDigest           string `json:"ref_digest"`
	Subtype             string `json:"subtype,omitempty"`
	Status              string `json:"status"`
	JurisdictionCode    string `json:"jurisdiction_code,omitempty"`
	InForce             bool   `json:"in_force"`
	Reason              string `json:"reason,omitempty"`
	Used                bool   `json:"used"`
	SourceEventID       string `json:"source_event_id"`
	SourceObjectVersion int64  `json:"source_object_version"`
}

// InputFact records a supplied fact the engine could not use to decide.
type InputFact struct {
	Fact   string `json:"fact"`
	Value  string `json:"value"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

const inputRecordedNotDeciding = "RECORDED_NOT_DECIDING"

// ValidSupplyType and ValidBusinessStatus report whether the classification is accepted.
func ValidSupplyType(s string) bool     { return inList(SupplyTypes, s) }
func ValidBusinessStatus(s string) bool { return inList(BusinessStatuses, s) }

// DecideJurisdiction is DetermineJurisdiction extended with the entity's establishments and registrations
// and the transaction's own facts. facts are the entity's projected facts (CurrentFacts already applied).
func DecideJurisdiction(assignments []Assignment, purpose, claimed string, at time.Time, facts []FactView, in FactInputs) Determination {
	det := DetermineJurisdiction(assignments, purpose, claimed, at)
	step := func(f string, a ...any) { det.Explanation = append(det.Explanation, fmt.Sprintf(f, a...)) }
	det.Inputs = recordInputs(in)
	for _, i := range det.Inputs {
		step("%s was supplied and is recorded; it does not decide (%s)", i.Fact, i.Note)
	}
	if det.Outcome == OutcomeNoApplicable || det.Outcome == OutcomeClaimNotAuthorised {
		return det
	}

	candidate := func(code string) int {
		for i, c := range det.Candidates {
			if strings.EqualFold(c.JurisdictionCode, code) {
				return i
			}
		}
		return -1
	}
	find := func(kind, id string) *FactView {
		for i := range facts {
			if facts[i].Kind == kind && facts[i].ID == id {
				return &facts[i]
			}
		}
		return nil
	}
	used := map[string]bool{}
	unusable := func(fact, reason string) Determination {
		det.Outcome, det.Basis, det.Jurisdiction = OutcomeFactNotUsable, "", nil
		det.MissingFacts = []MissingFact{{Fact: fact, Reason: reason}}
		step("%s cannot be used: %s. Nothing is guessed in its place", fact, reason)
		det.Facts = factEvidence(facts, used)
		return det
	}

	type selection struct{ fact, code string }
	var sels []selection
	if claimed != "" && (det.Claim.Status == ClaimConfirmed || det.Claim.Status == ClaimSelectedAmongAuthorised) {
		sels = append(sels, selection{FactNameClaimed, strings.ToUpper(claimed)})
	}
	for _, f := range []struct{ kind, name, id string }{
		{FactEstablishment, FactNameEstablishmentID, strings.TrimSpace(in.EstablishmentID)},
		{FactRegistration, FactNameRegistrationID, strings.TrimSpace(in.RegistrationID)},
	} {
		if f.id == "" {
			continue
		}
		v := find(f.kind, f.id)
		switch {
		case v == nil:
			return unusable(f.name, "no such "+strings.ToLower(f.kind)+" is known for this entity on this date")
		case !v.InForce:
			return unusable(f.name, "the "+strings.ToLower(f.kind)+" is not in force: "+v.Reason)
		case v.Code == "":
			return unusable(f.name, "the jurisdiction of the "+strings.ToLower(f.kind)+" is not known to this service")
		case candidate(v.Code) < 0:
			return unusable(f.name, fmt.Sprintf("its jurisdiction %s is not one the entity is authorised for", v.Code))
		}
		used[f.kind+"\x00"+f.id] = true
		sels = append(sels, selection{f.name, v.Code})
		step("%s is in force and points at %s, an authorised jurisdiction", f.name, v.Code)
	}
	det.Facts = factEvidence(facts, used)

	codes := map[string]bool{}
	for _, s := range sels {
		codes[s.code] = true
	}
	switch {
	case len(codes) > 1:
		var parts []string
		for _, s := range sels {
			parts = append(parts, s.fact+"="+s.code)
		}
		det.Outcome, det.Basis, det.Jurisdiction = OutcomeFactsConflict, "", nil
		step("the supplied facts point at different jurisdictions (%s): nothing is chosen between them", strings.Join(parts, ", "))
	case len(codes) == 1 && det.Outcome == OutcomeJurisdictionAmbiguous:
		c := det.Candidates[candidate(sels[0].code)]
		det.Outcome, det.Basis, det.Jurisdiction = OutcomeJurisdictionDetermined, BasisFactsWithinAuthorised, &c
		step("the entity is authorised for %d jurisdictions; the supplied facts select %s from among them", len(det.Candidates), c.JurisdictionCode)
	case det.Outcome == OutcomeJurisdictionAmbiguous:
		det.Outcome = OutcomeInsufficientFacts
		for _, v := range facts {
			if !v.InForce || v.Code == "" || candidate(v.Code) < 0 {
				continue
			}
			name := FactNameEstablishmentID
			if v.Kind == FactRegistration {
				name = FactNameRegistrationID
			}
			if !hasMissing(det.MissingFacts, name) {
				det.MissingFacts = append(det.MissingFacts, MissingFact{Fact: name,
					Reason: "the entity has a " + strings.ToLower(v.Kind) + " in force in a candidate jurisdiction; naming the one this transaction belongs to would select it"})
			}
		}
		det.MissingFacts = append(det.MissingFacts, MissingFact{Fact: FactNameClaimed,
			Reason: "a jurisdiction among the candidates; it chooses inside the authorised set and never adds to it"})
		step("more than one jurisdiction applies and no supplied fact selects between them: the decision needs %s", missingNames(det.MissingFacts))
	}
	return det
}

func hasMissing(m []MissingFact, name string) bool {
	for _, x := range m {
		if x.Fact == name {
			return true
		}
	}
	return false
}

func missingNames(m []MissingFact) string {
	n := make([]string, len(m))
	for i, x := range m {
		n[i] = x.Fact
	}
	return strings.Join(n, " or ")
}

func recordInputs(in FactInputs) []InputFact {
	const note = "place-of-supply rules are not built, so this fact is not used to decide"
	var out []InputFact
	if in.Counterparty != nil {
		if in.Counterparty.Country != "" {
			out = append(out, InputFact{Fact: "counterparty.country", Value: strings.ToUpper(in.Counterparty.Country), Status: inputRecordedNotDeciding, Note: note})
		}
		if in.Counterparty.BusinessStatus != "" {
			out = append(out, InputFact{Fact: "counterparty.business_status", Value: in.Counterparty.BusinessStatus, Status: inputRecordedNotDeciding, Note: note})
		}
	}
	if in.Transaction != nil && in.Transaction.SupplyType != "" {
		out = append(out, InputFact{Fact: "transaction.supply_type", Value: in.Transaction.SupplyType, Status: inputRecordedNotDeciding, Note: note})
	}
	return out
}

func factEvidence(facts []FactView, used map[string]bool) []FactEvidence {
	out := make([]FactEvidence, 0, len(facts))
	for _, v := range facts {
		out = append(out, FactEvidence{Kind: v.Kind, RefDigest: IdentifierDigest(v.Kind + ":" + v.ID), Subtype: v.Subtype, Status: v.Status,
			JurisdictionCode: v.Code, InForce: v.InForce, Reason: v.Reason, Used: used[v.Kind+"\x00"+v.ID],
			SourceEventID: v.SourceEventID, SourceObjectVersion: v.Version})
	}
	return out
}

// RegistryFactEvent is one consumed establishment or registration status event, ready to project.
type RegistryFactEvent struct {
	TenantID      string
	LegalEntityID string
	Row           FactRow
}

// AddressCountryEvent is one consumed address event's effect on the address-country projection. Country is empty
// when the event does not say (a superseded version).
type AddressCountryEvent struct {
	TenantID  string
	AddressID string
	Country   string
	Version   int64
}
