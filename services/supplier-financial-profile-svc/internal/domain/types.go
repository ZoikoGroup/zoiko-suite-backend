// Package domain defines the canonical types for supplier-financial-profile-svc
// — AP-01, "Supplier Financial Profile", from the Procurement/Expenses/
// Accounts Payable engineering baseline. The foundational service of the
// payment side of Accounts Payable: AP-03/05 (commitments, invoices) must
// refuse inactive or held suppliers via the eligibility read, and AP-09/10/11
// (Payment Proposal/Authorization/Run) pay against a governed profile.
//
// Scope and honest boundaries:
//
//   - AP-01 holds only a payee/banking identity REFERENCE, never raw banking
//     data. The authoritative owner is ORG-10 (payee-banking-identity-svc).
//     GetPayeeReference resolves the CURRENT active ORG-10 destination
//     (/org10/parties/{ref}/active) and returns its id/version; it fails
//     closed (503) when ORG-10 is down and answers 404 when ORG-10 has no
//     active destination. It never falls back to a stored, stale or
//     invoice-printed value, so a consumer comparing invoice bank details
//     against the "payee master" compares against ORG-10's own answer
//     (negative path #1). Changes to the stored payee_reference field are
//     high-risk and maker-checker controlled here; changing the bank
//     details themselves stays in ORG-10.
//   - Every change bumps Version, writes an append-only revision (full
//     before/after snapshot, actor, approver, reason, effective instant) and
//     the domain events to the transactional outbox in the SAME transaction.
//     GetSupplierFinancialProfileAsOf reconstructs the profile (and the
//     payment terms in force) at any past instant from those revisions.
//   - Payment-terms periods are effective-dated and non-overlapping, enforced
//     by a Postgres EXCLUDE constraint (negative path #2).
//   - High-risk fields (payee_reference, payment_method_preference, AP account
//     policy, tax/withholding classification, risk/control flags) are never
//     applied directly: they are proposed and applied only when a DIFFERENT
//     principal approves (authorization-svc own-object SoD, plus a local
//     maker!=checker guard). The principal who last changed payee-related
//     fields is exposed (last-payee-change) so AP-10 can refuse to let that
//     principal authorize the resulting payment (negative path #3; the
//     enforcement half lives in AP-10).
//   - Eligibility for new commitments (ACTIVE and not on hold) is exposed for
//     AP-03/AP-05 (negative path #4).
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ── Profile ──────────────────────────────────────────────────────────────────

type ProfileStatus string

const (
	StatusDraft     ProfileStatus = "DRAFT"
	StatusActive    ProfileStatus = "ACTIVE"
	StatusOnHold    ProfileStatus = "ON_HOLD"
	StatusSuspended ProfileStatus = "SUSPENDED"
	StatusRetired   ProfileStatus = "RETIRED"
)

var profileTransitions = map[ProfileStatus][]ProfileStatus{
	StatusDraft:     {StatusActive, StatusRetired},
	StatusActive:    {StatusOnHold, StatusSuspended, StatusRetired},
	StatusOnHold:    {StatusActive, StatusRetired},
	StatusSuspended: {StatusActive, StatusRetired},
}

func ValidProfileTransition(from, to ProfileStatus) bool {
	for _, allowed := range profileTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// SupplierFinancialProfile is the current-state row — AP-01's own
// "Authoritative ownership" list, minus the parts that belong to other
// registries (party/legal identity, jurisdiction facts, the real payee
// identity). Version increments on every change; UpdatedAt moves with it
// (downstream services use UpdatedAt as the payee "version").
type SupplierFinancialProfile struct {
	ProfileID               string        `json:"profile_id"`
	TenantID                *string       `json:"tenant_id,omitempty"`
	LegalEntityID           string        `json:"legal_entity_id"`
	SupplierRef             string        `json:"supplier_ref"` // party/role ID from the party master
	Status                  ProfileStatus `json:"status"`
	Version                 int           `json:"version"`
	PayeeReference          string        `json:"payee_reference,omitempty"` // controlled ORG-10 party reference (high-risk field)
	Category                string        `json:"category,omitempty"`
	ProcurementCategoryRefs []string      `json:"procurement_category_refs,omitempty"`
	InvoiceChannel          string        `json:"invoice_channel,omitempty"`
	PaymentMethodPreference string        `json:"payment_method_preference,omitempty"` // high-risk field
	TaxWithholdingRef       string        `json:"tax_withholding_ref,omitempty"`       // high-risk field
	TaxClassificationRefs   []string      `json:"tax_classification_refs,omitempty"`   // high-risk field
	APAccountPolicy         string        `json:"ap_account_policy,omitempty"`         // high-risk field
	RiskControlFlags        []string      `json:"risk_control_flags,omitempty"`        // high-risk field
	HoldReason              string        `json:"hold_reason,omitempty"`               // reason for ON_HOLD or SUSPENDED
	CreatedAt               time.Time     `json:"created_at"`
	CreatedByPrincipalID    string        `json:"created_by_principal_id"`
	UpdatedAt               time.Time     `json:"updated_at"`
}

// ── Payment terms (effective-dated, non-overlapping) ────────────────────────

// PaymentTermsPeriod is append-only: a correction is a new period, never
// an edit of a prior one. Non-overlapping effective periods per profile
// are enforced at the DATABASE layer via a Postgres EXCLUDE constraint on
// a date range.
type PaymentTermsPeriod struct {
	PaymentTermsID       string     `json:"payment_terms_id"`
	TenantID             *string    `json:"tenant_id,omitempty"`
	ProfileID            string     `json:"profile_id"`
	TermsCode            string     `json:"terms_code"` // data only, e.g. NET_30, NET_60, DUE_ON_RECEIPT
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"` // nil = open-ended
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
}

// ── High-risk change proposals (own-object SoD) ─────────────────────────────

// HighRiskField names the fields whose change requires independent approval
// — AP-01's own SoD line. PAYEE_REFERENCE and PAYMENT_METHOD_PREFERENCE are
// "payee-related": the principal who changed them is exposed to AP-10.
type HighRiskField string

const (
	FieldPayeeReference          HighRiskField = "PAYEE_REFERENCE"
	FieldPaymentMethodPreference HighRiskField = "PAYMENT_METHOD_PREFERENCE"
	FieldAPAccountPolicy         HighRiskField = "AP_ACCOUNT_POLICY"
	FieldTaxWithholdingRef       HighRiskField = "TAX_WITHHOLDING_REF"
	FieldTaxClassificationRefs   HighRiskField = "TAX_CLASSIFICATION_REFS"
	FieldRiskControlFlags        HighRiskField = "RISK_CONTROL_FLAGS"
)

func (f HighRiskField) Valid() bool {
	switch f {
	case FieldPayeeReference, FieldPaymentMethodPreference, FieldAPAccountPolicy,
		FieldTaxWithholdingRef, FieldTaxClassificationRefs, FieldRiskControlFlags:
		return true
	}
	return false
}

// IsPayeeRelated reports whether a change to f is a payee-related change for
// the purpose of the "bank changer cannot authorize the payment" control.
func (f HighRiskField) IsPayeeRelated() bool {
	return f == FieldPayeeReference || f == FieldPaymentMethodPreference
}

func isListField(f HighRiskField) bool {
	return f == FieldTaxClassificationRefs || f == FieldRiskControlFlags
}

// HighRiskFieldValue renders the current value of f on p as the string form
// stored in a change request (lists are JSON arrays).
func HighRiskFieldValue(p SupplierFinancialProfile, f HighRiskField) string {
	switch f {
	case FieldPayeeReference:
		return p.PayeeReference
	case FieldPaymentMethodPreference:
		return p.PaymentMethodPreference
	case FieldAPAccountPolicy:
		return p.APAccountPolicy
	case FieldTaxWithholdingRef:
		return p.TaxWithholdingRef
	case FieldTaxClassificationRefs:
		return listToString(p.TaxClassificationRefs)
	case FieldRiskControlFlags:
		return listToString(p.RiskControlFlags)
	}
	return ""
}

func listToString(l []string) string {
	if len(l) == 0 {
		return ""
	}
	b, _ := json.Marshal(l)
	return string(b)
}

// ValidateHighRiskValue checks that value is well-formed for f (list fields
// carry a JSON array of strings) so an approved change can always be applied.
func ValidateHighRiskValue(f HighRiskField, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: new_value is required", ErrValidation)
	}
	if isListField(f) {
		var l []string
		if err := json.Unmarshal([]byte(value), &l); err != nil {
			return fmt.Errorf("%w: new_value for %s must be a JSON array of strings", ErrValidation, f)
		}
	}
	return nil
}

// ApplyHighRiskField sets f on p to value.
func ApplyHighRiskField(p *SupplierFinancialProfile, f HighRiskField, value string) error {
	if err := ValidateHighRiskValue(f, value); err != nil {
		return err
	}
	switch f {
	case FieldPayeeReference:
		p.PayeeReference = value
	case FieldPaymentMethodPreference:
		p.PaymentMethodPreference = value
	case FieldAPAccountPolicy:
		p.APAccountPolicy = value
	case FieldTaxWithholdingRef:
		p.TaxWithholdingRef = value
	case FieldTaxClassificationRefs:
		var l []string
		_ = json.Unmarshal([]byte(value), &l)
		p.TaxClassificationRefs = l
	case FieldRiskControlFlags:
		var l []string
		_ = json.Unmarshal([]byte(value), &l)
		p.RiskControlFlags = l
	}
	return nil
}

type ChangeRequestStatus string

const (
	ChangeRequestPending  ChangeRequestStatus = "PENDING_APPROVAL"
	ChangeRequestApproved ChangeRequestStatus = "APPROVED"
	ChangeRequestRejected ChangeRequestStatus = "REJECTED"
)

// HighRiskChangeRequest is the evidence of one proposed change; deciding it
// records who decided it and why on the same row.
type HighRiskChangeRequest struct {
	ChangeRequestID       string              `json:"change_request_id"`
	TenantID              *string             `json:"tenant_id,omitempty"`
	ProfileID             string              `json:"profile_id"`
	Field                 HighRiskField       `json:"field"`
	OldValue              string              `json:"old_value,omitempty"`
	NewValue              string              `json:"new_value"`
	Reason                string              `json:"reason,omitempty"`
	Status                ChangeRequestStatus `json:"status"`
	ProposedByPrincipalID string              `json:"proposed_by_principal_id"`
	ProposedAt            time.Time           `json:"proposed_at"`
	DecidedByPrincipalID  *string             `json:"decided_by_principal_id,omitempty"`
	DecidedAt             *time.Time          `json:"decided_at,omitempty"`
	DecisionReason        string              `json:"decision_reason,omitempty"`
}

// ── Evidence ─────────────────────────────────────────────────────────────────

// ProfileChangeEvent is append-only evidence of every state-affecting
// action — AP-01's own "Evidence / lineage" line.
type ProfileChangeEvent struct {
	EventID          string    `json:"event_id"`
	TenantID         *string   `json:"tenant_id,omitempty"`
	ProfileID        string    `json:"profile_id"`
	EventType        string    `json:"event_type"` // data only — see the Event* constants below
	PriorValue       string    `json:"prior_value,omitempty"`
	NewValue         string    `json:"new_value,omitempty"`
	Reason           string    `json:"reason,omitempty"`
	ActorPrincipalID string    `json:"actor_principal_id"`
	CreatedAt        time.Time `json:"created_at"`
}

const (
	EventProfileCreated      = "PROFILE_CREATED"
	EventProfileActivated    = "PROFILE_ACTIVATED"
	EventPaymentTermsChanged = "PAYMENT_TERMS_CHANGED"
	EventHoldPlaced          = "HOLD_PLACED"
	EventHoldReleased        = "HOLD_RELEASED"
	EventProfileSuspended    = "PROFILE_SUSPENDED"
	EventProfileUnsuspended  = "PROFILE_UNSUSPENDED"
	EventHighRiskProposed    = "HIGH_RISK_CHANGE_PROPOSED"
	EventHighRiskApplied     = "HIGH_RISK_CHANGE_APPLIED"
	EventHighRiskRejected    = "HIGH_RISK_CHANGE_REJECTED"
	EventProfileAmended      = "PROFILE_AMENDED"
	EventProfileRetired      = "PROFILE_RETIRED"
)

// ProfileRevision is one append-only history row: the full profile state
// after (Snapshot) and before (PriorSnapshot) a change. A revision is in
// force from EffectiveFrom until the next revision's EffectiveFrom.
type ProfileRevision struct {
	RevisionID          string                    `json:"revision_id"`
	ProfileID           string                    `json:"profile_id"`
	Version             int                       `json:"version"`
	ChangeType          string                    `json:"change_type"`
	Snapshot            SupplierFinancialProfile  `json:"snapshot"`
	PriorSnapshot       *SupplierFinancialProfile `json:"prior_snapshot,omitempty"`
	ActorPrincipalID    string                    `json:"actor_principal_id"`
	ApproverPrincipalID string                    `json:"approver_principal_id,omitempty"`
	Reason              string                    `json:"reason,omitempty"`
	PayeeRelated        bool                      `json:"payee_related"`
	EffectiveFrom       time.Time                 `json:"effective_from"`
	EffectiveTo         *time.Time                `json:"effective_to,omitempty"`
}

// ProfileAsOf is the reconstruction returned by GetSupplierFinancialProfileAsOf.
type ProfileAsOf struct {
	Profile         SupplierFinancialProfile `json:"profile"`
	AsOf            time.Time                `json:"as_of"`
	RevisionVersion int                      `json:"revision_version"`
	EffectiveFrom   time.Time                `json:"effective_from"`
	PaymentTerms    *PaymentTermsPeriod      `json:"payment_terms,omitempty"` // the period in force at AsOf, if any
}

// LastPayeeChange identifies who last changed payee-related fields, so AP-10
// can refuse to let that principal authorize the resulting payment.
type LastPayeeChange struct {
	ProfileID           string    `json:"profile_id"`
	PrincipalID         string    `json:"principal_id"`
	ApproverPrincipalID string    `json:"approver_principal_id,omitempty"`
	ChangedAt           time.Time `json:"changed_at"`
	Version             int       `json:"version"`
	ChangeType          string    `json:"change_type"`
}

// Eligibility is the AP-03/AP-05 read contract: may new commitments (POs,
// invoices) be raised against this supplier?
type Eligibility struct {
	ProfileID                 string        `json:"profile_id"`
	SupplierRef               string        `json:"supplier_ref"`
	LegalEntityID             string        `json:"legal_entity_id"`
	Status                    ProfileStatus `json:"status"`
	Version                   int           `json:"version"`
	IsOnHold                  bool          `json:"is_on_hold"`
	EligibleForNewCommitments bool          `json:"eligible_for_new_commitments"`
	Reason                    string        `json:"reason"`
}

// EligibilityOf derives eligibility from a profile: only ACTIVE (and not on
// hold) suppliers are eligible.
func EligibilityOf(p SupplierFinancialProfile) Eligibility {
	e := Eligibility{
		ProfileID: p.ProfileID, SupplierRef: p.SupplierRef, LegalEntityID: p.LegalEntityID,
		Status: p.Status, Version: p.Version, IsOnHold: p.Status == StatusOnHold,
	}
	switch p.Status {
	case StatusActive:
		e.EligibleForNewCommitments = true
	case StatusOnHold:
		e.Reason = "supplier is on hold"
	case StatusSuspended:
		e.Reason = "supplier is suspended"
	case StatusRetired:
		e.Reason = "supplier profile is retired"
	default:
		e.Reason = "supplier profile is not active (" + string(p.Status) + ")"
	}
	if e.Reason != "" && p.HoldReason != "" && (p.Status == StatusOnHold || p.Status == StatusSuspended) {
		e.Reason += ": " + p.HoldReason
	}
	return e
}

// ── State-transition commands (pure) ─────────────────────────────────────────

type Command string

const (
	CmdActivate    Command = "ACTIVATE"
	CmdPlaceHold   Command = "PLACE_HOLD"
	CmdReleaseHold Command = "RELEASE_HOLD"
	CmdSuspend     Command = "SUSPEND"
	CmdUnsuspend   Command = "UNSUSPEND"
	CmdRetire      Command = "RETIRE"
)

// Outcome describes a change for the revision/evidence/outbox writers.
type Outcome struct {
	ChangeType   string // revision change_type, also selects the outbox event names
	EventType    string // profile_change_events type
	Prior, New   string
	Reason       string
	PayeeRelated bool
}

// ApplyCommand applies a status command to a copy of cur.
func ApplyCommand(cur SupplierFinancialProfile, cmd Command, reason string) (SupplierFinancialProfile, Outcome, error) {
	next := cur
	prior := string(cur.Status)
	switch cmd {
	case CmdActivate:
		if cur.Status != StatusDraft {
			return cur, Outcome{}, fmt.Errorf("%w: profile is not in the required DRAFT state", ErrInvalidTransition)
		}
		next.Status = StatusActive
		return next, Outcome{ChangeType: ChangeActivated, EventType: EventProfileActivated, Prior: prior, New: "ACTIVE", Reason: reason}, nil
	case CmdPlaceHold:
		if cur.Status != StatusActive {
			return cur, Outcome{}, fmt.Errorf("%w: profile is not in the required ACTIVE state", ErrInvalidTransition)
		}
		if strings.TrimSpace(reason) == "" {
			return cur, Outcome{}, fmt.Errorf("%w: reason is required", ErrValidation)
		}
		next.Status, next.HoldReason = StatusOnHold, reason
		return next, Outcome{ChangeType: ChangeHoldPlaced, EventType: EventHoldPlaced, Prior: prior, New: "ON_HOLD", Reason: reason}, nil
	case CmdReleaseHold:
		if cur.Status != StatusOnHold {
			return cur, Outcome{}, fmt.Errorf("%w: profile is not in the required ON_HOLD state", ErrInvalidTransition)
		}
		next.Status, next.HoldReason = StatusActive, ""
		return next, Outcome{ChangeType: ChangeHoldReleased, EventType: EventHoldReleased, Prior: prior, New: "ACTIVE", Reason: reason}, nil
	case CmdSuspend:
		if cur.Status != StatusActive {
			return cur, Outcome{}, fmt.Errorf("%w: profile is not in the required ACTIVE state", ErrInvalidTransition)
		}
		if strings.TrimSpace(reason) == "" {
			return cur, Outcome{}, fmt.Errorf("%w: reason is required", ErrValidation)
		}
		next.Status, next.HoldReason = StatusSuspended, reason
		return next, Outcome{ChangeType: ChangeSuspended, EventType: EventProfileSuspended, Prior: prior, New: "SUSPENDED", Reason: reason}, nil
	case CmdUnsuspend:
		if cur.Status != StatusSuspended {
			return cur, Outcome{}, fmt.Errorf("%w: profile is not in the required SUSPENDED state", ErrInvalidTransition)
		}
		next.Status, next.HoldReason = StatusActive, ""
		return next, Outcome{ChangeType: ChangeUnsuspended, EventType: EventProfileUnsuspended, Prior: prior, New: "ACTIVE", Reason: reason}, nil
	case CmdRetire:
		if !ValidProfileTransition(cur.Status, StatusRetired) {
			return cur, Outcome{}, fmt.Errorf("%w: profile is already RETIRED", ErrInvalidTransition)
		}
		next.Status = StatusRetired
		return next, Outcome{ChangeType: ChangeRetired, EventType: EventProfileRetired, Prior: prior, New: "RETIRED", Reason: reason}, nil
	}
	return cur, Outcome{}, fmt.Errorf("%w: unknown command %q", ErrValidation, cmd)
}

// Revision change types. They also select the outbox event names.
const (
	ChangeCreated             = "CREATED"
	ChangeActivated           = "ACTIVATED"
	ChangeAmended             = "AMENDED"
	ChangeHoldPlaced          = "HOLD_PLACED"
	ChangeHoldReleased        = "HOLD_RELEASED"
	ChangeSuspended           = "SUSPENDED"
	ChangeUnsuspended         = "UNSUSPENDED"
	ChangeRetired             = "RETIRED"
	ChangePaymentTermsChanged = "PAYMENT_TERMS_CHANGED"
	ChangeHighRiskApplied     = "HIGH_RISK_CHANGE_APPLIED"
)

// Spec event names (§4 "Events produced") and the legacy lowercase names that
// pre-date them — still emitted as aliases because consumers may rely on them.
const (
	EvtProfileCreated      = "SupplierFinancialProfileCreated"
	EvtProfileChanged      = "SupplierFinancialProfileChanged"
	EvtHoldPlaced          = "SupplierHoldPlaced"
	EvtHoldReleased        = "SupplierHoldReleased"
	EvtPaymentTermsChanged = "SupplierPaymentTermsChanged"

	LegacyProfileCreated      = "supplier_financial_profile.created"
	LegacyHoldPlaced          = "supplier_hold.placed"
	LegacyHoldReleased        = "supplier_hold.released"
	LegacyPaymentTermsChanged = "supplier_payment_terms.changed"
	LegacyHighRiskDecided     = "supplier_high_risk_change.decided"
)

// OutboxEventTypes returns the event type strings (spec name first, then any
// legacy alias) to emit for a revision of the given change type.
func OutboxEventTypes(changeType string) []string {
	switch changeType {
	case ChangeCreated:
		return []string{EvtProfileCreated, LegacyProfileCreated}
	case ChangeHoldPlaced:
		return []string{EvtHoldPlaced, LegacyHoldPlaced}
	case ChangeHoldReleased:
		return []string{EvtHoldReleased, LegacyHoldReleased}
	case ChangePaymentTermsChanged:
		return []string{EvtPaymentTermsChanged, LegacyPaymentTermsChanged}
	}
	return []string{EvtProfileChanged}
}

// ── Request DTOs ─────────────────────────────────────────────────────────────

type CreateProfileRequest struct {
	TenantID                string   `json:"tenant_id,omitempty"`
	LegalEntityID           string   `json:"legal_entity_id"`
	SupplierRef             string   `json:"supplier_ref"`
	Category                string   `json:"category,omitempty"`
	InvoiceChannel          string   `json:"invoice_channel,omitempty"`
	ProcurementCategoryRefs []string `json:"procurement_category_refs,omitempty"`
}

// AmendProfileRequest covers the spec's amendable fields. The first group is
// applied immediately; the second group is HIGH-RISK and is turned into
// pending change requests (maker-checker) instead of being applied.
type AmendProfileRequest struct {
	ExpectedVersion *int `json:"expected_version,omitempty"`

	Category                *string   `json:"category,omitempty"`
	InvoiceChannel          *string   `json:"invoice_channel,omitempty"`
	ProcurementCategoryRefs *[]string `json:"procurement_category_refs,omitempty"`

	TaxWithholdingRef     *string   `json:"tax_withholding_ref,omitempty"`
	TaxClassificationRefs *[]string `json:"tax_classification_refs,omitempty"`
	APAccountPolicy       *string   `json:"ap_account_policy,omitempty"`
	RiskControlFlags      *[]string `json:"risk_control_flags,omitempty"`

	Reason string `json:"reason,omitempty"`
}

// LowRiskPresent reports whether any immediately-applied field is set.
func (r AmendProfileRequest) LowRiskPresent() bool {
	return r.Category != nil || r.InvoiceChannel != nil || r.ProcurementCategoryRefs != nil
}

// HighRiskProposals returns one proposal per high-risk field present.
func (r AmendProfileRequest) HighRiskProposals() []ProposeHighRiskChangeRequest {
	var out []ProposeHighRiskChangeRequest
	add := func(f HighRiskField, v string) {
		out = append(out, ProposeHighRiskChangeRequest{Field: f, NewValue: v, Reason: r.Reason})
	}
	if r.TaxWithholdingRef != nil {
		add(FieldTaxWithholdingRef, *r.TaxWithholdingRef)
	}
	if r.TaxClassificationRefs != nil {
		add(FieldTaxClassificationRefs, listToStringForceArray(*r.TaxClassificationRefs))
	}
	if r.APAccountPolicy != nil {
		add(FieldAPAccountPolicy, *r.APAccountPolicy)
	}
	if r.RiskControlFlags != nil {
		add(FieldRiskControlFlags, listToStringForceArray(*r.RiskControlFlags))
	}
	return out
}

func listToStringForceArray(l []string) string {
	if l == nil {
		l = []string{}
	}
	b, _ := json.Marshal(l)
	return string(b)
}

// ApplyAmendLowRisk applies only the immediately-applied amend fields.
func ApplyAmendLowRisk(cur SupplierFinancialProfile, req AmendProfileRequest) (SupplierFinancialProfile, Outcome, error) {
	if cur.Status == StatusRetired {
		return cur, Outcome{}, fmt.Errorf("%w: profile is RETIRED", ErrInvalidTransition)
	}
	next := cur
	if req.Category != nil {
		next.Category = *req.Category
	}
	if req.InvoiceChannel != nil {
		next.InvoiceChannel = *req.InvoiceChannel
	}
	if req.ProcurementCategoryRefs != nil {
		next.ProcurementCategoryRefs = *req.ProcurementCategoryRefs
	}
	return next, Outcome{ChangeType: ChangeAmended, EventType: EventProfileAmended, Reason: req.Reason}, nil
}

// AmendResult is the amend response: the (possibly unchanged) profile and any
// pending maker-checker requests created for high-risk fields.
type AmendResult struct {
	Profile        SupplierFinancialProfile `json:"profile"`
	PendingChanges []HighRiskChangeRequest  `json:"pending_change_requests,omitempty"`
}

type ChangePaymentTermsRequest struct {
	ExpectedVersion *int       `json:"expected_version,omitempty"`
	TermsCode       string     `json:"terms_code"`
	EffectiveFrom   time.Time  `json:"effective_from"`
	EffectiveTo     *time.Time `json:"effective_to,omitempty"`
}

type PlaceHoldRequest struct {
	ExpectedVersion *int   `json:"expected_version,omitempty"`
	Reason          string `json:"reason"`
}

// TransitionRequest is the body of release-hold / suspend / unsuspend /
// activate (all optional except Reason on suspend).
type TransitionRequest struct {
	ExpectedVersion *int   `json:"expected_version,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

type ProposeHighRiskChangeRequest struct {
	ExpectedVersion *int          `json:"expected_version,omitempty"`
	Field           HighRiskField `json:"field"`
	NewValue        string        `json:"new_value"`
	Reason          string        `json:"reason,omitempty"`
}

// ChangePaymentMethodPreferenceRequest is the body of the
// ChangePaymentMethodPreference command (a high-risk, maker-checker change).
type ChangePaymentMethodPreferenceRequest struct {
	ExpectedVersion *int   `json:"expected_version,omitempty"`
	NewValue        string `json:"new_value"`
	Reason          string `json:"reason,omitempty"`
}

type DecideHighRiskChangeRequest struct {
	// ExpectedVersion is the profile version the approver reviewed; REQUIRED.
	ExpectedVersion *int   `json:"expected_version,omitempty"`
	Approve         bool   `json:"approve"`
	Reason          string `json:"reason,omitempty"`
}

type RetireProfileRequest struct {
	ExpectedVersion *int   `json:"expected_version,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// ── idempotency ──────────────────────────────────────────────────────────────

// IdemScope identifies one Idempotency-Key command execution. The store
// records the response in the SAME transaction as the state change.
type IdemScope struct {
	Key         string
	Operation   string
	RequestHash string
}

// IdemRecord is a stored command result.
type IdemRecord struct {
	Operation   string
	RequestHash string
	StatusCode  int
	Response    json.RawMessage
}

// ── sentinel errors ──────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrProfileNotFound         = errorString("supplier financial profile not found")
	ErrInvalidTransition       = errorString("invalid profile status transition")
	ErrOverlappingPaymentTerms = errorString("payment terms period overlaps an existing effective period")
	ErrChangeRequestNotFound   = errorString("high-risk change request not found")
	ErrChangeRequestNotPending = errorString("high-risk change request is not pending")
	ErrStoreUnavailable        = errorString("supplier-financial-profile store unavailable")
	ErrStaleVersion            = errorString("expected_version does not match the current profile version")
	ErrValidation              = errorString("validation failed")
	ErrNoChanges               = errorString("amend request contains no changes")
	ErrDuplicateProfile        = errorString("a live supplier financial profile already exists for this supplier and legal entity")
	ErrInvalidTenant           = errorString("tenant id is not valid")
	ErrNoRevisionAsOf          = errorString("no profile revision existed at the requested time")
	ErrNoPayeeChange           = errorString("no payee-related change has been recorded for this profile")
	ErrIdempotencyRace         = errorString("concurrent request with the same Idempotency-Key")
)

// ErrSoDConflict: the deciding principal is the proposer (maker == checker).
const ErrSoDConflict = errorString("segregation of duties: a principal cannot approve their own high-risk change")

// IsDomain reports whether err is one of this package's sentinel errors.
func IsDomain(err error) bool {
	var e errorString
	return errors.As(err, &e)
}

// HTTPStatus is 202 when high-risk fields were routed to maker-checker, else 200.
func (r AmendResult) HTTPStatus() int {
	if len(r.PendingChanges) > 0 {
		return 202
	}
	return 200
}

// Body keeps the pre-existing wire shape (a bare profile) unless pending
// change requests were created.
func (r AmendResult) Body() any {
	if len(r.PendingChanges) > 0 {
		return r
	}
	return r.Profile
}
