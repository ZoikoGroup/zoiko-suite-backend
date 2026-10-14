// Package ar01 implements AR-01 Customer Financial Profile (spec ZS-SVC-C-001
// section 4): entity-scoped customer financial, billing, credit and collection
// settings. It owns financial terms only; party legal identity/address stay in
// the party master (counterparty-management-svc) and are never copied here.
package ar01

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Authorization actions (spec: customer.financial.manage, customer.credit.limit.manage/approve,
// customer.credit.hold.manage). Reads are tenant-scoped only (settled AR read posture).
const (
	ActionProfileManage = "AR01_PROFILE_MANAGE"
	ActionLimitManage   = "AR01_CREDIT_LIMIT_MANAGE"
	ActionLimitApprove  = "AR01_CREDIT_LIMIT_APPROVE"
	ActionHoldManage    = "AR01_CREDIT_HOLD_MANAGE"
)

// Codes local to AR-01 beyond the shared spec section 14 classes.
const (
	CodeProfileExists   = "CUSTOMER_PROFILE_EXISTS"
	CodeVersionOverlap  = "PROFILE_VERSION_OVERLAP"
	CodeLimitPending    = "CREDIT_LIMIT_CHANGE_PENDING"
	AggregateType       = "CustomerFinancialProfile"
	PolicyRefLimitMax   = "credit_limit_policy_max"
	dateLayout          = "2006-01-02"
	maxHistoryPageLimit = 200
)

// Event types (one dotted lowercase scheme: ar.<aggregate>.<verb>).
const (
	EventProfileCreated       = "ar.customer_financial_profile.created"
	EventProfileChanged       = "ar.customer_financial_profile.changed"
	EventCreditLimitChanged   = "ar.customer_credit_limit.changed"
	EventCreditLimitRequested = "ar.customer_credit_limit.change_requested"
	EventCreditLimitRejected  = "ar.customer_credit_limit.change_rejected"
	EventCreditHoldPlaced     = "ar.customer_credit_hold.placed"
	EventCreditHoldReleased   = "ar.customer_credit_hold.released"
)

// State is the profile lifecycle: Draft -> Active -> Suspended/OnHold -> Retired.
type State string

const (
	StateDraft     State = "DRAFT"
	StateActive    State = "ACTIVE"
	StateOnHold    State = "ON_HOLD"
	StateSuspended State = "SUSPENDED"
	StateRetired   State = "RETIRED"
)

// Transition names a lifecycle move.
type Transition string

const (
	TrActivate  Transition = "ACTIVATE"
	TrSuspend   Transition = "SUSPEND"
	TrReinstate Transition = "REINSTATE"
	TrHold      Transition = "PLACE_HOLD"
	TrRelease   Transition = "RELEASE_HOLD"
	TrRetire    Transition = "RETIRE"
)

var transitions = map[Transition]map[State]State{
	TrActivate:  {StateDraft: StateActive},
	TrSuspend:   {StateActive: StateSuspended},
	TrReinstate: {StateSuspended: StateActive},
	TrHold:      {StateActive: StateOnHold},
	TrRelease:   {StateOnHold: StateActive},
	TrRetire:    {StateDraft: StateRetired, StateActive: StateRetired, StateOnHold: StateRetired, StateSuspended: StateRetired},
}

// Next applies a transition or returns 409 INVALID_STATE_TRANSITION.
func Next(cur State, t Transition) (State, error) {
	if to, ok := transitions[t][cur]; ok {
		return to, nil
	}
	return "", kit.Conflict(kit.CodeInvalidTransition, fmt.Sprintf("%s is not allowed from state %s", t, cur))
}

// Resolvable reports whether a profile in this state may be used for material
// issuance (an ON_HOLD profile resolves, flagged CreditHold, so the caller can
// answer CREDIT_HOLD_ACTIVE; SUSPENDED/DRAFT/RETIRED never resolve).
func (s State) Resolvable() bool { return s == StateActive || s == StateOnHold }

// ── value objects ────────────────────────────────────────────────────────────

type InvoicePrefs struct {
	DeliveryChannel string `json:"delivery_channel,omitempty"` // EMAIL|PORTAL|POST|EDI
	Language        string `json:"language,omitempty"`
	Format          string `json:"format,omitempty"` // PDF|UBL|PEPPOL|...
	Consolidated    bool   `json:"consolidated,omitempty"`
	PORequired      bool   `json:"po_required,omitempty"`
}

type StatementPrefs struct {
	Frequency       string `json:"frequency,omitempty"` // NONE|WEEKLY|MONTHLY|QUARTERLY
	DeliveryChannel string `json:"delivery_channel,omitempty"`
}

// RiskAttributes are approved customer risk/credit attributes with provenance.
type RiskAttributes struct {
	Rating     string `json:"rating,omitempty"` // LOW|MEDIUM|HIGH|RESTRICTED
	Score      *int   `json:"score,omitempty"`
	Source     string `json:"source,omitempty"`
	SourceRef  string `json:"source_ref,omitempty"`
	AssessedOn string `json:"assessed_on,omitempty"`
}

func (r RiskAttributes) IsZero() bool { return r == RiskAttributes{} }

// Attributes is the versioned attribute set (everything but payment terms).
type Attributes struct {
	CreditLimit          kit.Amount     `json:"credit_limit"`
	CreditCurrency       string         `json:"credit_currency"`
	InvoicePreferences   InvoicePrefs   `json:"invoice_preferences"`
	StatementPreferences StatementPrefs `json:"statement_preferences"`
	DunningProfile       string         `json:"dunning_profile"`
	ARAccountPolicy      string         `json:"ar_account_policy"`
	RiskAttributes       RiskAttributes `json:"risk_attributes"`
}

var (
	codeRe     = regexp.MustCompile(`^[A-Z0-9_]{1,32}$`)
	policyRe   = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,64}$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
)

func oneOf(v string, allowed ...string) bool {
	if v == "" {
		return true
	}
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// Validate is the pure validation of an attribute set.
func (a Attributes) Validate() error {
	if !currencyRe.MatchString(a.CreditCurrency) {
		return kit.Invalid("credit_currency must be a 3-letter upper-case ISO 4217 code")
	}
	if a.CreditLimit.IsNegative() {
		return kit.Invalid("credit_limit must not be negative")
	}
	if !a.CreditLimit.FitsCurrency(a.CreditCurrency) {
		return kit.Invalid("credit_limit has more precision than the currency allows")
	}
	if !oneOf(a.InvoicePreferences.DeliveryChannel, "EMAIL", "PORTAL", "POST", "EDI") {
		return kit.Invalid("invoice_preferences.delivery_channel must be EMAIL, PORTAL, POST or EDI")
	}
	if !oneOf(a.StatementPreferences.Frequency, "NONE", "WEEKLY", "MONTHLY", "QUARTERLY") {
		return kit.Invalid("statement_preferences.frequency must be NONE, WEEKLY, MONTHLY or QUARTERLY")
	}
	if !oneOf(a.StatementPreferences.DeliveryChannel, "EMAIL", "PORTAL", "POST", "EDI") {
		return kit.Invalid("statement_preferences.delivery_channel must be EMAIL, PORTAL, POST or EDI")
	}
	for name, v := range map[string]string{"dunning_profile": a.DunningProfile, "ar_account_policy": a.ARAccountPolicy} {
		if v != "" && !policyRe.MatchString(v) {
			return kit.Invalid(name + " must be 1-64 characters of letters, digits, '_', '-' or '.'")
		}
	}
	return a.RiskAttributes.Validate()
}

// Validate requires provenance for any asserted risk attribute (spec: risk-source provenance).
func (r RiskAttributes) Validate() error {
	if r.IsZero() {
		return nil
	}
	if !oneOf(r.Rating, "LOW", "MEDIUM", "HIGH", "RESTRICTED") {
		return kit.Invalid("risk_attributes.rating must be LOW, MEDIUM, HIGH or RESTRICTED")
	}
	if strings.TrimSpace(r.Source) == "" {
		return kit.Invalid("risk_attributes.source is required: risk attributes must carry their source/provenance")
	}
	if r.Score != nil && (*r.Score < 0 || *r.Score > 1000) {
		return kit.Invalid("risk_attributes.score must be between 0 and 1000")
	}
	if r.AssessedOn != "" {
		if _, err := time.Parse(dateLayout, r.AssessedOn); err != nil {
			return kit.Invalid("risk_attributes.assessed_on must be YYYY-MM-DD")
		}
	}
	return nil
}

// ValidateTerms checks payment terms.
func ValidateTerms(code string, days int) error {
	if !codeRe.MatchString(code) {
		return kit.Invalid("payment_terms_code must be 1-32 characters of A-Z, 0-9 or '_'")
	}
	if days < 0 || days > 365 {
		return kit.Invalid("payment_terms_days must be between 0 and 365")
	}
	return nil
}

// ParseDay parses YYYY-MM-DD.
func ParseDay(s string) (time.Time, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return time.Time{}, kit.Invalid("dates must be YYYY-MM-DD")
	}
	return t, nil
}

func day(t time.Time) string { return t.UTC().Format(dateLayout) }

func mustUUID(field, v string) error {
	if _, err := uuid.Parse(v); err != nil {
		return kit.Invalid(field + " must be a UUID")
	}
	return nil
}

// ── persisted shapes ─────────────────────────────────────────────────────────

// Header is the stable profile (one per tenant, legal entity, customer party).
type Header struct {
	ProfileID       string     `json:"profile_id"`
	TenantID        string     `json:"tenant_id"`
	LegalEntityID   string     `json:"legal_entity_id"`
	CustomerPartyID string     `json:"customer_party_id"`
	State           State      `json:"state"`
	CreditHold      bool       `json:"credit_hold"`
	HoldReasonCode  string     `json:"hold_reason_code,omitempty"`
	HoldPlacedBy    string     `json:"hold_placed_by,omitempty"`
	HoldPlacedAt    *time.Time `json:"hold_placed_at,omitempty"`
	PolicyMax       kit.Amount `json:"credit_limit_policy_max"`
	CreatedBy       string     `json:"created_by"`
	Version         int        `json:"version"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// AttrVersion is one recorded attribute version. Its effective interval is
// [EffectiveFrom, next version's EffectiveFrom); the latest is open-ended.
type AttrVersion struct {
	Seq           int    `json:"seq"`
	EffectiveFrom string `json:"effective_from"`
	EffectiveTo   string `json:"effective_to,omitempty"`
	Attributes
	ChangeKind        string    `json:"change_kind"`
	ReasonCode        string    `json:"reason_code,omitempty"`
	EvidenceID        string    `json:"evidence_id,omitempty"`
	CreatedBy         string    `json:"created_by"`
	ApprovedBy        string    `json:"approved_by,omitempty"`
	ApprovalRequestID string    `json:"approval_request_id,omitempty"`
	RecordedAt        time.Time `json:"recorded_at"`
}

// TermVersion is one effective-dated payment-terms version with an explicit interval.
type TermVersion struct {
	Seq           int       `json:"seq"`
	Code          string    `json:"payment_terms_code"`
	Days          int       `json:"payment_terms_days"`
	EffectiveFrom string    `json:"effective_from"`
	EffectiveTo   string    `json:"effective_to,omitempty"`
	ReasonCode    string    `json:"reason_code,omitempty"`
	EvidenceID    string    `json:"evidence_id,omitempty"`
	CreatedBy     string    `json:"created_by"`
	RecordedAt    time.Time `json:"recorded_at"`
}

// LimitRequest is a credit-limit increase awaiting a different principal's approval.
type LimitRequest struct {
	RequestID      string     `json:"request_id"`
	RequestedLimit kit.Amount `json:"requested_limit"`
	Currency       string     `json:"currency"`
	PriorLimit     kit.Amount `json:"prior_limit"`
	PolicyMax      kit.Amount `json:"policy_max_at_request"`
	RequestedBy    string     `json:"requested_by"`
	ReasonCode     string     `json:"reason_code"`
	EvidenceID     string     `json:"evidence_id,omitempty"`
	Status         string     `json:"status"`
	DecidedBy      string     `json:"decided_by,omitempty"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	DecisionReason string     `json:"decision_reason,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Profile is the read model: the header plus what was effective on the date asked.
type Profile struct {
	Header
	AsOf                 string        `json:"as_of,omitempty"`
	PaymentTerms         *TermVersion  `json:"payment_terms"`
	Attributes           *AttrVersion  `json:"attributes"`
	UpcomingTerms        []TermVersion `json:"upcoming_payment_terms,omitempty"`
	UpcomingAttributes   []AttrVersion `json:"upcoming_attributes,omitempty"`
	PendingCreditRequest *LimitRequest `json:"pending_credit_limit_request,omitempty"`
}

// HistoryEntry is one row of the append-only profile history.
type HistoryEntry struct {
	ProfileVersion  int            `json:"profile_version"`
	Command         string         `json:"command"`
	StateAfter      State          `json:"state_after"`
	CreditHoldAfter bool           `json:"credit_hold_after"`
	ActorID         string         `json:"actor_id"`
	ApproverID      string         `json:"approver_id,omitempty"`
	ReasonCode      string         `json:"reason_code,omitempty"`
	EvidenceID      string         `json:"evidence_id,omitempty"`
	EffectiveFrom   string         `json:"effective_from,omitempty"`
	PriorValues     map[string]any `json:"prior_values,omitempty"`
	NewValues       map[string]any `json:"new_values,omitempty"`
	PolicyRef       string         `json:"policy_ref,omitempty"`
	CorrelationID   string         `json:"correlation_id,omitempty"`
	RecordedAt      time.Time      `json:"recorded_at"`
}

// ── errors ───────────────────────────────────────────────────────────────────

func errProfileInactive(msg string) *kit.Problem {
	return kit.Unprocessable(kit.CodeCustomerProfileInactive, msg)
}

func errOverlap(msg string) *kit.Problem {
	return kit.Err(http.StatusConflict, CodeVersionOverlap, msg)
}

// PartyUsable decides whether the party master's answer permits financial
// mutation: it must be an ACTIVE CUSTOMER that has not ended.
func PartyUsable(p *Party, tenantID string, today time.Time) error {
	if p == nil {
		return errProfileInactive("customer party not found")
	}
	if p.TenantID != "" && p.TenantID != tenantID {
		return errProfileInactive("customer party not found")
	}
	if !strings.EqualFold(p.Type, "CUSTOMER") {
		return errProfileInactive("party is not a CUSTOMER counterparty")
	}
	if !strings.EqualFold(p.Status, "ACTIVE") {
		return errProfileInactive("customer party is not ACTIVE (status " + p.Status + ")")
	}
	if p.EffectiveTo != "" {
		if end, err := time.Parse(dateLayout, p.EffectiveTo[:min(len(p.EffectiveTo), 10)]); err == nil && !end.After(today.Truncate(24*time.Hour)) {
			return errProfileInactive("customer party effective period has ended")
		}
	}
	return nil
}
