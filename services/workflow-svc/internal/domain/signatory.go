package domain

import (
	"encoding/json"
	"time"
)

// Signature handoff from policy-svc (ZS-GOV-INT-002 §4.2, ZS-GOV-POL-001 §5).
//
// policy-svc resolves WHAT a signature needs (distinct signers, corporate
// offices, order, the exact document); this service collects WHO signs, as a
// QUORUM stage whose votes are the signatures. Authorization decides, at the
// moment of each signature, which offices the signer holds. Nothing here
// decides corporate authority on its own.

// SignatoryActionPrefix + office is the authorization action that says a
// principal may sign as that corporate office in a legal entity
// (SIGNATORY_SIGN_AS_DIRECTOR, ...). An office no role grants cannot be
// signed for: the check fails closed.
const SignatoryActionPrefix = "SIGNATORY_SIGN_AS_"

// SignatoryBinding pins the policy-svc decision a workflow was created from.
type SignatoryBinding struct {
	WorkflowInstanceID string          `json:"workflow_instance_id"`
	TenantID           string          `json:"tenant_id"`
	DecisionID         string          `json:"signatory_decision_id"`
	PolicyVersionID    string          `json:"signatory_policy_version_id"`
	RuleID             string          `json:"signatory_rule_id"`
	DocumentHash       string          `json:"document_hash"`
	Requirement        json.RawMessage `json:"requirement"`
	BoundAt            time.Time       `json:"bound_at"`
}

// OfficeSeat is a number of distinct signers who must hold one office.
type OfficeSeat struct {
	Office   string `json:"office"`
	MinCount int    `json:"min_count"`
}

// SignatureRequirement is the SIGNATURE_QUORUM required action of a
// policy-svc decision, as this service enforces it.
type SignatureRequirement struct {
	Kind               string       `json:"kind"`
	MinDistinctSigners int          `json:"min_distinct_signers"`
	RequiredOffices    []OfficeSeat `json:"required_offices,omitempty"`
	EligibleOffices    []string     `json:"eligible_offices,omitempty"`
	Sequencing         string       `json:"sequencing,omitempty"`
	Order              []string     `json:"order,omitempty"`
	Constraints        []string     `json:"constraints,omitempty"`
	DocumentHash       string       `json:"document_hash,omitempty"`
}

// ParseSignatureRequirement decodes a binding's requirement.
func ParseSignatureRequirement(raw json.RawMessage) (*SignatureRequirement, error) {
	var r SignatureRequirement
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// AcceptedOffices are the offices a signature may be counted for: the
// required ones and the eligible ones, in a stable order, each once.
func (r *SignatureRequirement) AcceptedOffices() []string {
	seen := map[string]bool{}
	var out []string
	add := func(o string) {
		if o != "" && !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	for _, o := range r.Order {
		add(o)
	}
	for _, s := range r.RequiredOffices {
		add(s.Office)
	}
	for _, o := range r.EligibleOffices {
		add(o)
	}
	return out
}

// Signer is one signature already given: who, and the offices Authorization
// confirmed they held when they signed.
type Signer struct {
	PrincipalID string
	Offices     []string
}

// seats expands the office requirements into single seats.
func (r *SignatureRequirement) seats() []string {
	var out []string
	for _, s := range r.RequiredOffices {
		for i := 0; i < s.MinCount; i++ {
			out = append(out, s.Office)
		}
	}
	return out
}

// OfficesFilled reports whether distinct signers can fill every required
// office seat, each signer filling at most one seat (bipartite matching): a
// person who is both CFO and CEO is still one signer (Doc 02 C02-P0-06).
func (r *SignatureRequirement) OfficesFilled(signers []Signer) bool {
	seats := r.seats()
	if len(seats) == 0 {
		return true
	}
	if len(signers) < len(seats) {
		return false
	}
	holds := func(s Signer, office string) bool {
		for _, o := range s.Offices {
			if o == office {
				return true
			}
		}
		return false
	}
	seatOf := make([]int, len(signers)) // signer -> seat, -1 none
	for i := range seatOf {
		seatOf[i] = -1
	}
	signerOf := make([]int, len(seats)) // seat -> signer, -1 none
	for i := range signerOf {
		signerOf[i] = -1
	}
	var augment func(seat int, visited []bool) bool
	augment = func(seat int, visited []bool) bool {
		for s := range signers {
			if visited[s] || !holds(signers[s], seats[seat]) {
				continue
			}
			visited[s] = true
			if seatOf[s] == -1 || augment(seatOf[s], visited) {
				seatOf[s], signerOf[seat] = seat, s
				return true
			}
		}
		return false
	}
	for seat := range seats {
		if !augment(seat, make([]bool, len(signers))) {
			return false
		}
	}
	return true
}

// NextInOrder is the office the next signature must be given in, for a
// SEQUENTIAL requirement: the first office of Order whose seats the earlier
// signers have not filled yet. "" when the order is complete (or the
// requirement is parallel), so any accepted office may sign.
func (r *SignatureRequirement) NextInOrder(signers []Signer) string {
	if r.Sequencing != "SEQUENTIAL" || len(r.Order) == 0 {
		return ""
	}
	need := map[string]int{}
	for _, s := range r.RequiredOffices {
		need[s.Office] = s.MinCount
	}
	used := make([]bool, len(signers))
	for _, office := range r.Order {
		want := need[office]
		if want == 0 {
			want = 1
		}
		got := 0
		for i, s := range signers {
			if used[i] {
				continue
			}
			for _, o := range s.Offices {
				if o == office {
					used[i] = true
					got++
					break
				}
			}
			if got == want {
				break
			}
		}
		if got < want {
			return office
		}
	}
	return ""
}

// Signature handoff errors.
var (
	ErrSignatureDocumentChanged = errorString("the document signed is not the document the signature requirement is bound to")
	ErrSignatureOutOfOrder      = errorString("this signature is out of the required order")
	ErrSignerNotEligible        = errorString("the signer holds none of the offices this signature requirement accepts")
)
