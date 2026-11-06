// Package claims resolves P1 high-impact facts (price, trial, market
// availability, certification, residency, integration status) from the
// signed claim registry (Doc 03 §08, Doc 05 §17). The model never sees a
// claim value it could paraphrase into a new fact: verified claims carry
// their approved public statement, which is used verbatim.
package claims

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// placeholderRe matches an unfilled blank in approved wording, such as
// "[price]", "[N]" or "{{plan}}".
var placeholderRe = regexp.MustCompile(`\[[A-Za-z][A-Za-z0-9 _.-]{0,30}\]|\{\{[^{}]*\}\}`)

// HasPlaceholder reports whether s still contains an unfilled blank. Such
// text is never shown to a visitor (Doc 02 §17) and a claim worded that way
// carries no typed value (Doc 03 evidence gates).
func HasPlaceholder(s string) bool { return placeholderRe.MatchString(s) }

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
	ReasonIncomplete     = "CLAIM_INCOMPLETE"
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
	// OwnerID is the accountable team (Doc 03 §10 RACI); an ACTIVE claim
	// must have one. ReviewDueAt: past it the claim is treated as expired
	// until re-reviewed (Doc 03 §11, KB-028).
	OwnerID                  string     `json:"owner_id"`
	ReviewDueAt              *time.Time `json:"review_due_at,omitempty"`
	PriceBookRelease         string     `json:"price_book_release,omitempty"`
	TaxesFeesCaveat          string     `json:"taxes_fees_caveat,omitempty"`
	TrialCohort              string     `json:"trial_cohort,omitempty"`
	TrialDurationDays        int        `json:"trial_duration_days,omitempty"`
	TrialCardRequired        *bool      `json:"trial_card_required,omitempty"`
	TrialEndState            string     `json:"trial_end_state,omitempty"`
	TrialAuthorizationPolicy string     `json:"trial_authorization_policy,omitempty"`
	// Synthetic marks seed data that is not ZoikoSuite fact.
	Synthetic bool `json:"synthetic,omitempty"`
}

func (c Claim) Complete() bool {
	switch strings.ToUpper(strings.TrimSpace(c.ClaimType)) {
	case Price:
		return strings.TrimSpace(c.SubjectID) != "" &&
			strings.TrimSpace(c.Plan) != "" &&
			strings.TrimSpace(c.Currency) != "" &&
			strings.TrimSpace(c.BillingTerm) != "" &&
			strings.TrimSpace(c.Market) != "" &&
			!c.ValidFrom.IsZero() &&
			(strings.TrimSpace(c.PriceBookRelease) != "" || strings.TrimSpace(c.AuthorityRef) != "") &&
			strings.TrimSpace(c.TaxesFeesCaveat) != ""
	case Trial:
		return strings.TrimSpace(c.SubjectID) != "" &&
			strings.TrimSpace(c.Plan) != "" &&
			strings.TrimSpace(c.Market) != "" &&
			strings.TrimSpace(c.TrialCohort) != "" &&
			c.TrialDurationDays > 0 &&
			c.TrialCardRequired != nil &&
			strings.TrimSpace(c.TrialEndState) != "" &&
			strings.TrimSpace(c.TrialAuthorizationPolicy) != ""
	default:
		return true
	}
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
	// CompatiblePolicyVersions: see knowledge.Manifest (Doc 08 OPR-004).
	CompatiblePolicyVersions []string `json:"compatible_policy_versions,omitempty"`
}

// HasSynthetic reports whether the registry or any claim is synthetic. A
// production runtime refuses such a release unless local/dev allows it.
func (m Manifest) HasSynthetic() bool {
	if m.Synthetic {
		return true
	}
	for _, c := range m.Claims {
		if c.Synthetic {
			return true
		}
	}
	return false
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

// evidenceRevoker is implemented by revocation sets that also know which
// evidence source versions were withdrawn (KB-030).
type evidenceRevoker interface {
	EvidenceRevoked(sourceVersionID string) bool
}

func evidenceRevoked(rev RevocationSet, c Claim) bool {
	er, ok := rev.(evidenceRevoker)
	if !ok {
		return false
	}
	for _, v := range c.EvidenceSourceVersionIDs {
		if er.EvidenceRevoked(v) {
			return true
		}
	}
	return false
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
	sawRevoked, sawExpired, sawConflict, sawIncomplete := false, false, false, false
	for _, c := range r.m.Claims {
		if !r.matches(c, q) {
			continue
		}
		switch {
		case rev != nil && (rev.ClaimRevoked(c.ClaimID) || evidenceRevoked(rev, c)):
			sawRevoked = true
		case c.Status != StatusActive || !c.PubliclyPublishable || !c.OperationallyEnabled:
			// HOLD/TARGET/RETIRED/CONDITIONAL or unpublished: never stated as fact.
		case q.AsOf.Before(c.ValidFrom) || !q.AsOf.Before(c.ValidUntil) || (c.ReviewDueAt != nil && !q.AsOf.Before(*c.ReviewDueAt)):
			sawExpired = true
		case len(c.ConflictIDs) > 0 || (c.ClaimType == Price && r.disputed[c.SubjectID]):
			sawConflict = true
			d.ConflictKeys = append(d.ConflictKeys, c.ClaimID)
		case strings.TrimSpace(c.PublicStatement) == "" || len(c.EvidenceSourceVersionIDs) == 0 || c.CitationURLKey == "":
			// No approved wording or provenance: cannot be cited.
		case (!c.Synthetic && !c.Complete()) || HasPlaceholder(c.PublicStatement):
			// A blank such as "[price]" is not a value, even in synthetic data.
			sawIncomplete = true
		default:
			eligible = append(eligible, c)
		}
	}
	// Two active values for the same key that disagree → CONTRADICTED. Every
	// eligible claim is in force at AsOf, so their windows overlap.
	byKey := map[string]string{}
	for _, c := range eligible {
		k := ConflictKey(c)
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
	case sawIncomplete:
		d.Decision, d.ReasonCode = InsufficientEvidence, ReasonIncomplete
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

// ClaimConflict is a set of ACTIVE claims for one key whose values disagree
// while their validity windows overlap (KB-005). SubjectKey starts with the
// claim type.
type ClaimConflict struct {
	SubjectKey string
	ClaimIDs   []string
	OwnerID    string
}

// ConflictKey is the scope two claims must share to contradict each other
// (Doc 03 §16, Doc 05 §17): a different currency, plan or locale is a
// different fact, not a conflict.
func ConflictKey(c Claim) string {
	return strings.Join([]string{c.ClaimType, c.SubjectID, c.Predicate, c.Market, c.BillingTerm, c.Segment, c.Currency, c.Plan, c.Locale}, "|")
}

// windowsOverlap reports whether two [from, until) validity windows share an
// instant. A zero until is open-ended.
func windowsOverlap(a, b Claim) bool {
	return (a.ValidUntil.IsZero() || b.ValidFrom.Before(a.ValidUntil)) &&
		(b.ValidUntil.IsZero() || a.ValidFrom.Before(b.ValidUntil))
}

// Conflicts lists every key with two or more ACTIVE claims that disagree in
// overlapping validity windows, in key order, with the first named owner
// among them. A value that replaces another at a later date is succession,
// not a conflict.
func (m Manifest) Conflicts() []ClaimConflict {
	groups := map[string][]Claim{}
	for _, c := range m.Claims {
		if c.Status != StatusActive {
			continue
		}
		k := ConflictKey(c)
		groups[k] = append(groups[k], c)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []ClaimConflict
	for _, k := range keys {
		g := groups[k]
		in := map[int]bool{}
		for i := range g {
			for j := i + 1; j < len(g); j++ {
				if g[i].NormalizedValue != g[j].NormalizedValue && windowsOverlap(g[i], g[j]) {
					in[i], in[j] = true, true
				}
			}
		}
		if len(in) == 0 {
			continue
		}
		cc := ClaimConflict{SubjectKey: k}
		for i, c := range g {
			if !in[i] {
				continue
			}
			cc.ClaimIDs = append(cc.ClaimIDs, c.ClaimID)
			if cc.OwnerID == "" {
				cc.OwnerID = c.OwnerID
			}
		}
		sort.Strings(cc.ClaimIDs)
		out = append(out, cc)
	}
	return out
}

// Validate checks a manifest before it is signed: the registry structure
// plus per-claim completeness (Doc 03 §17).
func (m Manifest) Validate() []string {
	problems := m.StructuralProblems()
	for _, c := range m.Claims {
		if c.ClaimID != "" && c.Status == StatusActive && !c.Synthetic && !c.Complete() {
			problems = append(problems, "ACTIVE price/trial claim missing typed fields (Doc 03 §17): "+c.ClaimID)
		}
	}
	return problems
}

// StructuralProblems are the registry-integrity failures a runtime refuses
// at load (Doc 03 §6/§12, CT-043): missing identity, duplicate ids, missing
// two-person approval or owner, an empty validity window. An incomplete
// price/trial claim is not one of them: the resolver already abstains on
// that claim alone (CLAIM_INCOMPLETE), so it never takes the release down.
func (m Manifest) StructuralProblems() []string {
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
		case c.Status == StatusActive && strings.TrimSpace(c.OwnerID) == "":
			problems = append(problems, "ACTIVE claim needs an accountable owner (Doc 03 §10): "+c.ClaimID)
		case c.Status == StatusActive && !c.ValidUntil.After(c.ValidFrom):
			problems = append(problems, "invalid validity window: "+c.ClaimID)
		}
		seen[c.ClaimID] = true
	}
	return problems
}
