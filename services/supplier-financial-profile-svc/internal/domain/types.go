// Package domain defines the canonical types for supplier-financial-profile-svc
// — AP-01, "Supplier Financial Profile", from the Procurement/Expenses/
// Accounts Payable engineering baseline. The foundational service of the
// unbuilt payment side of Accounts Payable: AP-09/10/11 (Payment
// Proposal/Authorization/Run) all need a governed supplier financial
// profile to pay against, same relationship PRV-01 had to the rest of
// the privacy domain.
//
// v1 scope, documented rather than hidden (same doctrine as the privacy
// domain services):
//
//   - AP-01's own contract names "ORG-10 Banking Identity/Payee Master"
//     as the authoritative owner of actual bank/payee details — AP-01
//     itself only holds "a payee/banking identity REFERENCE and
//     version," never the raw banking data. No such ORG-10 service
//     exists anywhere in this codebase (confirmed: no payee/banking-
//     identity/beneficiary-master service of any name).
//     `banking-connector-svc` is a different bounded context entirely
//     (bank statement ingestion for reconciliation, not a payee identity
//     master for payment routing). PayeeReference is therefore an
//     OPAQUE, caller-supplied string — recorded and versioned, never
//     resolved or validated against a real registry — same doctrine as
//     PRV-01's `lawful_basis_refs` and retention-registry-svc's
//     `record_class`.
//   - The spec's own negative-path acceptance list names four scenarios.
//     Two are built for real here: #2 ("supplier payment terms overlap
//     effective periods") is enforced with a genuine Postgres EXCLUDE
//     constraint on a date range, not application-level date-math that
//     could race; a variant of #3 ("bank changer attempts to authorize
//     supplier payment") is enforced as an own-object Segregation-of-
//     Duties check — the principal who proposed a change to
//     payee_reference or payment_method_preference cannot be the one who
//     approves it, using authorization-svc's dynamic SoD layer (built
//     earlier in this platform's history) as the FIRST real caller of
//     that feature. #1 ("invoice bank details differ from payee master")
//     and the enforcement half of #4 ("inactive supplier used in new
//     PO/invoice") both require a DOMAIN SERVICE THAT CONSUMES this
//     profile (invoice-approval-svc, purchase-order-svc) to actually
//     check it before proceeding — neither of those services calls this
//     one today, and wiring that is a separate, future integration this
//     service does not perform on their behalf.
//   - The spec's command table does not name an explicit "activate"
//     command, but the state model requires one (DRAFT is not directly
//     usable). ActivateSupplierFinancialProfile is added here as the
//     obvious missing transition, not fabricated business logic — a
//     profile is either created directly ACTIVE-eligible or explicitly
//     activated; nothing about what "active-worthy" means is invented.
package domain

import "time"

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
// (mostly unbuilt) registries: party/legal identity, jurisdiction/
// registration facts and the real payee identity all live elsewhere.
type SupplierFinancialProfile struct {
	ProfileID               string        `json:"profile_id"`
	TenantID                *string       `json:"tenant_id,omitempty"`
	LegalEntityID           string        `json:"legal_entity_id"`
	SupplierRef             string        `json:"supplier_ref"` // opaque party/role ID — see package doc comment
	Status                  ProfileStatus `json:"status"`
	PayeeReference          string        `json:"payee_reference,omitempty"` // opaque ORG-10 reference — see package doc comment
	Category                string        `json:"category,omitempty"`
	InvoiceChannel          string        `json:"invoice_channel,omitempty"`
	PaymentMethodPreference string        `json:"payment_method_preference,omitempty"`
	TaxWithholdingRef       string        `json:"tax_withholding_ref,omitempty"`
	HoldReason              string        `json:"hold_reason,omitempty"`
	CreatedAt               time.Time     `json:"created_at"`
	CreatedByPrincipalID    string        `json:"created_by_principal_id"`
	UpdatedAt               time.Time     `json:"updated_at"`
}

// ── Payment terms (effective-dated, non-overlapping) ────────────────────────

// PaymentTermsPeriod is append-only: a correction is a new period, never
// an edit of a prior one. Non-overlapping effective periods per profile
// are enforced at the DATABASE layer via a Postgres EXCLUDE constraint on
// a date range — negative-path scenario #2 from AP-01's own acceptance
// table, made a genuine invariant rather than an application-level
// check that a race could slip past.
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

// HighRiskField names the two fields whose change requires independent
// approval — AP-01's own SoD line: "users able to change payee identity
// cannot authorize resulting payment without independent control." This
// service cannot enforce the payment-authorization half (AP-10 doesn't
// exist), but it enforces the proposal/approval half directly: the
// proposer of one of these two fields cannot also approve it.
type HighRiskField string

const (
	FieldPayeeReference          HighRiskField = "PAYEE_REFERENCE"
	FieldPaymentMethodPreference HighRiskField = "PAYMENT_METHOD_PREFERENCE"
)

func (f HighRiskField) Valid() bool {
	return f == FieldPayeeReference || f == FieldPaymentMethodPreference
}

type ChangeRequestStatus string

const (
	ChangeRequestPending  ChangeRequestStatus = "PENDING_APPROVAL"
	ChangeRequestApproved ChangeRequestStatus = "APPROVED"
	ChangeRequestRejected ChangeRequestStatus = "REJECTED"
)

// HighRiskChangeRequest is append-only evidence of one proposed change —
// approving or rejecting it creates a new row's worth of state on THIS
// row (not a new row), since there is exactly one live request per
// proposal and its outcome is itself the evidence of what was decided,
// by whom, and why.
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

// ── Evidence log ─────────────────────────────────────────────────────────────

// ProfileChangeEvent is append-only evidence of every state-affecting
// action — AP-01's own "Evidence / lineage" line: "Prior/new values,
// effective interval, actor/approver, reason."
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
	EventHighRiskProposed    = "HIGH_RISK_CHANGE_PROPOSED"
	EventHighRiskApplied     = "HIGH_RISK_CHANGE_APPLIED"
	EventHighRiskRejected    = "HIGH_RISK_CHANGE_REJECTED"
	EventProfileAmended      = "PROFILE_AMENDED"
	EventProfileRetired      = "PROFILE_RETIRED"
)

// ── Request DTOs ─────────────────────────────────────────────────────────────

type CreateProfileRequest struct {
	TenantID       string `json:"tenant_id,omitempty"`
	LegalEntityID  string `json:"legal_entity_id"`
	SupplierRef    string `json:"supplier_ref"`
	Category       string `json:"category,omitempty"`
	InvoiceChannel string `json:"invoice_channel,omitempty"`
}

type AmendProfileRequest struct {
	Category       *string `json:"category,omitempty"`
	InvoiceChannel *string `json:"invoice_channel,omitempty"`
	Reason         string  `json:"reason,omitempty"`
}

type ChangePaymentTermsRequest struct {
	TermsCode     string     `json:"terms_code"`
	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`
}

type PlaceHoldRequest struct {
	Reason string `json:"reason"`
}

type ProposeHighRiskChangeRequest struct {
	Field    HighRiskField `json:"field"`
	NewValue string        `json:"new_value"`
	Reason   string        `json:"reason,omitempty"`
}

type DecideHighRiskChangeRequest struct {
	Approve bool   `json:"approve"`
	Reason  string `json:"reason,omitempty"`
}

type RetireProfileRequest struct {
	Reason string `json:"reason,omitempty"`
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
)
