// Package claims resolves P1 high-impact facts (price, trial, market
// availability, certification, residency, integration status) from the
// signed claim registry (Doc 03 §08, Doc 05 §17). The model never sees a
// claim value it could paraphrase into a new fact: verified claims carry
// their approved public statement, which is used verbatim.
package claims

import (
	"sort"
	"strings"
	"time"
)

const (
	KindClaims = "claims"
	MarketAll  = "ALL"
)

// Claim subjects.
const (
	Price              = "PRICE"
	Trial              = "TRIAL"
	MarketAvailability = "MARKET_AVAILABILITY"
	Certification      = "CERTIFICATION"
	Residency          = "RESIDENCY"
	IntegrationStatus  = "INTEGRATION_STATUS"
	// PlanAvailability states which plans are offered in a market.
	PlanAvailability = "PLAN_AVAILABILITY"
)

// Claim statuses (Doc 03 §08). Only ACTIVE claims are answerable.
const (
	StatusActive      = "ACTIVE"
	StatusConditional = "CONDITIONAL"
	StatusTarget      = "TARGET"
	StatusHold        = "HOLD"
	StatusRetired     = "RETIRED"
)

// Decisions (Doc 05 §17).
const (
	Verified             = "VERIFIED"
	InsufficientEvidence = "INSUFFICIENT_EVIDENCE"
	Contradicted         = "CONTRADICTED"
	RevokedDecision      = "REVOKED"
	ExpiredDecision      = "EXPIRED"
	MarketRequired       = "MARKET_REQUIRED" // not a §17 decision: tells the orchestrator to CLARIFY
)

// Claim is one registry entry. Absent scope means unknown, never global:
// market-neutral claims must say market "ALL" explicitly.
type Claim struct {
	ClaimID                  string    `json:"claim_id"`
	ClaimRevision            int       `json:"claim_revision"`
	ClaimType                string    `json:"claim_type"`
	SubjectID                string    `json:"subject_id"`
	Predicate                string    `json:"predicate"`
	NormalizedValue          string    `json:"normalized_value"`
	PublicStatement          string    `json:"public_statement"`
	Market                   string    `json:"market"`
	Plan                     string    `json:"plan"`
	Segment                  string    `json:"segment"`
	Locale                   string    `json:"locale"`
	BillingTerm              string    `json:"billing_term"`
	Currency                 string    `json:"currency"`
	Status                   string    `json:"status"`
	OperationallyEnabled     bool      `json:"operationally_enabled"`
	PubliclyPublishable      bool      `json:"publicly_publishable"`
	ValidFrom                time.Time `json:"valid_from"`
	ValidUntil               time.Time `json:"valid_until"`
	AuthorityRef             string    `json:"authority_ref"`
	EvidenceSourceVersionIDs []string  `json:"evidence_source_version_ids"`
	CitationURLKey           string    `json:"citation_url_key"`
	ApprovalRefs             []string  `json:"approval_refs"`
	ConflictIDs              []string  `json:"conflict_ids"`
	// Synthetic marks seed data that is not ZoikoSuite fact.
	Synthetic bool `json:"synthetic,omitempty"`
}

// Manifest is a signed claim release.
type Manifest struct {
	ReleaseID   string            `json:"release_id"`
	Kind        string            `json:"kind"`
	CreatedAt   time.Time         `json:"created_at"`
	ApprovedBy  []string          `json:"approved_by"`
	PlanAliases map[string]string `json:"plan_aliases"` // visitor word → plan code
	Markets     []string          `json:"markets"`      // markets offered as choices in config
	Claims      []Claim           `json:"claims"`
	Synthetic   bool              `json:"synthetic,omitempty"`
}

// Request asks for the claims about one subject.
type Request struct {
	Subject     string
	SubjectID   string // plan code, integration name or framework; "" = any
	Market      string
	BillingTerm string
	AsOf        time.Time
}

// Decision is the resolver outcome.
type Decision struct {
	Decision     string
	ReleaseID    string
	Verified     []Claim
	ReasonCode   string
	ConflictKeys []string // claim ids in conflict, for the conflict record
}

// RevocationSet reports revoked claim ids.
type RevocationSet interface {
	ClaimRevoked(claimID string) bool
}

// needsMarket lists subjects that are meaningless without a market.
var needsMarket = map[string]bool{Price: true, Trial: true, MarketAvailability: true, Residency: true, PlanAvailability: true}

// Resolver resolves requests against one verified manifest.
type Resolver struct {
	m Manifest
	// disputed are plan codes whose public page states a different price
	// than the signed claim (KB-003, A08): never quoted until resolved.
	disputed map[string]bool
}

func NewResolver(m Manifest) *Resolver { return &Resolver{m: m} }

func (r *Resolver) ReleaseID() string { return r.m.ReleaseID }

// Manifest returns the verified manifest.
func (r *Resolver) Manifest() Manifest { return r.m }

// WithDisputes returns a resolver that treats prices for these plan codes
// as contradicted.
func (r *Resolver) WithDisputes(plans map[string]bool) *Resolver {
	return &Resolver{m: r.m, disputed: plans}
}

// Disputed reports whether a plan's price is disputed by a public page.
func (r *Resolver) Disputed(plan string) bool { return r.disputed[plan] }

// PlanCode maps a visitor's plan word to a registry plan code.
func (r *Resolver) PlanCode(word string) string {
	return r.m.PlanAliases[strings.ToLower(word)]
}

// Markets returns the markets offered as choices.
func (r *Resolver) Markets() []string { return append([]string(nil), r.m.Markets...) }

// HasVerified reports whether any ACTIVE, valid, unrevoked claim of subject
// exists for market (used to gate routes such as signup_or_trial).
func (r *Resolver) HasVerified(subject, market string, now time.Time, rev RevocationSet) bool {
	d := r.Resolve(Request{Subject: subject, Market: market, AsOf: now}, rev)
	return d.Decision == Verified
}

func (r *Resolver) matches(c Claim, q Request) bool {
	if !strings.EqualFold(c.ClaimType, q.Subject) {
		return false
	}
	if q.SubjectID != "" && !strings.EqualFold(c.SubjectID, q.SubjectID) {
		return false
	}
	if needsMarket[q.Subject] {
		if c.Market == "" || c.Market != q.Market {
			return false
		}
	} else if c.Market != MarketAll && (q.Market == "" || c.Market != q.Market) {
		return false
	}
	if q.BillingTerm != "" && c.BillingTerm != "" && c.BillingTerm != q.BillingTerm {
		return false
	}
	return true
}

// Resolve applies the §17 decision rules. Precedence: a missing market asks
// for clarification; a contradiction always abstains; only then is a
// consistent, active, in-window, unrevoked set VERIFIED.
func (r *Resolver) Resolve(q Request, rev RevocationSet) Decision {
	d := Decision{ReleaseID: r.m.ReleaseID}
	if needsMarket[q.Subject] && q.Market == "" {
		d.Decision, d.ReasonCode = MarketRequired, "MARKET_REQUIRED"
		return d
	}
	var eligible []Claim
	sawRevoked, sawExpired, sawConflict := false, false, false
	for _, c := range r.m.Claims {
		if !r.matches(c, q) {
			continue
		}
		switch {
		case rev != nil && rev.ClaimRevoked(c.ClaimID):
			sawRevoked = true
		case c.Status != StatusActive || !c.PubliclyPublishable || !c.OperationallyEnabled:
			// HOLD/TARGET/RETIRED/CONDITIONAL or unpublished: never stated as fact.
		case q.AsOf.Before(c.ValidFrom) || !q.AsOf.Before(c.ValidUntil):
			sawExpired = true
		case len(c.ConflictIDs) > 0 || (c.ClaimType == Price && r.disputed[c.SubjectID]):
			sawConflict = true
			d.ConflictKeys = append(d.ConflictKeys, c.ClaimID)
		case strings.TrimSpace(c.PublicStatement) == "" || len(c.EvidenceSourceVersionIDs) == 0 || c.CitationURLKey == "":
			// No approved wording or provenance: cannot be cited.
		default:
			eligible = append(eligible, c)
		}
	}
	// Two active values for the same key that disagree → CONTRADICTED.
	byKey := map[string]string{}
	for _, c := range eligible {
		k := strings.Join([]string{c.ClaimType, c.SubjectID, c.Predicate, c.Market, c.BillingTerm, c.Segment}, "|")
		if v, ok := byKey[k]; ok && v != c.NormalizedValue {
			sawConflict = true
			d.ConflictKeys = append(d.ConflictKeys, c.ClaimID)
		}
		byKey[k] = c.NormalizedValue
	}
	switch {
	case sawConflict:
		d.Decision, d.ReasonCode = Contradicted, "CLAIM_CONFLICT"
		if len(d.ConflictKeys) == 0 {
			for _, c := range eligible {
				d.ConflictKeys = append(d.ConflictKeys, c.ClaimID)
			}
		}
		sort.Strings(d.ConflictKeys)
	case len(eligible) > 0:
		sort.Slice(eligible, func(i, j int) bool { return eligible[i].ClaimID < eligible[j].ClaimID })
		d.Decision, d.Verified = Verified, eligible
	case sawRevoked:
		d.Decision, d.ReasonCode = RevokedDecision, "SOURCE_REVOKED"
	case sawExpired:
		d.Decision, d.ReasonCode = ExpiredDecision, "EXPIRED_SOURCE"
	default:
		d.Decision, d.ReasonCode = InsufficientEvidence, "CLAIM_NOT_PUBLISHED"
	}
	return d
}

func distinct(xs []string) int {
	seen := map[string]bool{}
	for _, x := range xs {
		if x != "" {
			seen[x] = true
		}
	}
	return len(seen)
}

// Validate checks a manifest before it is signed.
func (m Manifest) Validate() []string {
	var problems []string
	seen := map[string]bool{}
	for _, c := range m.Claims {
		switch {
		case c.ClaimID == "" || c.ClaimType == "" || c.Status == "":
			problems = append(problems, "claim missing id/type/status: "+c.ClaimID)
		case seen[c.ClaimID]:
			problems = append(problems, "duplicate claim_id: "+c.ClaimID)
		case c.Status == StatusActive && distinct(c.ApprovalRefs) < 2:
			problems = append(problems, "ACTIVE claim needs two approvals (Doc 03 §10): "+c.ClaimID)
		case c.Status == StatusActive && !c.ValidUntil.After(c.ValidFrom):
			problems = append(problems, "invalid validity window: "+c.ClaimID)
		}
		seen[c.ClaimID] = true
	}
	return problems
}
