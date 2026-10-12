package domain

import "fmt"

// AP-05 orthogonal state dimensions (spec section 8, Figure 7).
//
// Intake, Match, Approval, Accounting, Settlement and Dispute/Hold are separate
// facts about a supplier invoice. They used to be folded into one generic
// `status` (RECEIVED -> VALIDATED -> APPROVED -> PAYMENT_REQUESTED), which made
// "approved but still disputed" or "validated but mismatched" unrepresentable and
// let a payment-side state (PAYMENT_REQUESTED) live in the invoice aggregate
// (invariant #10). Each dimension now has its own explicit transition table.
// The legacy `status` is derived from them (DeriveStatus) and is a summary only.

type IntakeState string

const (
	IntakeReceived    IntakeState = "RECEIVED"
	IntakeValidated   IntakeState = "VALIDATED"
	IntakeQuarantined IntakeState = "QUARANTINED"
	IntakeRejected    IntakeState = "REJECTED"
)

type MatchState string

const (
	MatchNotMatched      MatchState = "NOT_MATCHED"
	MatchMatched         MatchState = "MATCHED"
	MatchWithinTolerance MatchState = "WITHIN_TOLERANCE"
	MatchException       MatchState = "EXCEPTION"
	MatchIncomplete      MatchState = "INCOMPLETE"
)

type ApprovalState string

const (
	ApprovalNone     ApprovalState = "NONE"
	ApprovalPending  ApprovalState = "PENDING"
	ApprovalApproved ApprovalState = "APPROVED"
	ApprovalRejected ApprovalState = "REJECTED"
)

type AccountingState string

const (
	AccountingNotRequested AccountingState = "NOT_REQUESTED"
	AccountingRequested    AccountingState = "REQUESTED"
	AccountingPosted       AccountingState = "POSTED"
	AccountingFailed       AccountingState = "FAILED"
)

type SettlementState string

const (
	SettlementUnsettled         SettlementState = "UNSETTLED"
	SettlementPartiallySettled  SettlementState = "PARTIALLY_SETTLED"
	SettlementSettled           SettlementState = "SETTLED"
)

type HoldState string

const (
	HoldNone     HoldState = "NONE"
	HoldHeld     HoldState = "HELD"
	HoldDisputed HoldState = "DISPUTED"
)

// DuplicateState is the duplicate-risk sub-state that gates approval.
type DuplicateState string

const (
	DuplicateClear     DuplicateState = "CLEAR"
	DuplicateSuspected DuplicateState = "SUSPECTED" // near-duplicate: quarantined, unresolved
	DuplicateCleared   DuplicateState = "CLEARED"   // resolved as not a duplicate
	DuplicateConfirmed DuplicateState = "CONFIRMED" // resolved as a duplicate (rejected)
)

type TaxState string

const (
	TaxNotVerified TaxState = "NOT_VERIFIED"
	TaxVerified    TaxState = "VERIFIED"
	TaxChanged     TaxState = "CHANGED" // TAX result differs from the verified one
)

// PayeeState is the invoice-supplied bank data vs ORG-10 comparison.
type PayeeState string

const (
	PayeeNotProvided PayeeState = "NOT_PROVIDED"
	PayeeMatches     PayeeState = "MATCHES_ORG10"
	PayeeMismatch    PayeeState = "MISMATCH" // blocks approval until resolved
	PayeeResolved    PayeeState = "RESOLVED"
)

// Document types. Credit/debit documents are linked to the original and never
// mutate it.
const (
	DocInvoice    = "INVOICE"
	DocCreditNote = "CREDIT_NOTE"
	DocDebitNote  = "DEBIT_NOTE"
)

// ── transition tables ────────────────────────────────────────────────────────

var intakeTransitions = map[IntakeState][]IntakeState{
	IntakeReceived:    {IntakeValidated, IntakeQuarantined, IntakeRejected},
	IntakeValidated:   {IntakeQuarantined, IntakeRejected},
	IntakeQuarantined: {IntakeReceived, IntakeRejected},
	IntakeRejected:    {},
}

// A match result can be re-performed, so every result may follow every other;
// the table exists so that the set of legal values is still explicit.
var matchTransitions = map[MatchState][]MatchState{
	MatchNotMatched:      {MatchMatched, MatchWithinTolerance, MatchException, MatchIncomplete},
	MatchMatched:         {MatchMatched, MatchWithinTolerance, MatchException, MatchIncomplete, MatchNotMatched},
	MatchWithinTolerance: {MatchMatched, MatchWithinTolerance, MatchException, MatchIncomplete, MatchNotMatched},
	MatchException:       {MatchMatched, MatchWithinTolerance, MatchException, MatchIncomplete, MatchNotMatched},
	MatchIncomplete:      {MatchMatched, MatchWithinTolerance, MatchException, MatchIncomplete, MatchNotMatched},
}

var approvalTransitions = map[ApprovalState][]ApprovalState{
	ApprovalNone:     {ApprovalPending},
	ApprovalPending:  {ApprovalApproved, ApprovalRejected, ApprovalNone}, // None = approval invalidated
	ApprovalApproved: {},
	ApprovalRejected: {},
}

var accountingTransitions = map[AccountingState][]AccountingState{
	AccountingNotRequested: {AccountingRequested},
	AccountingRequested:    {AccountingPosted, AccountingFailed},
	AccountingFailed:       {AccountingRequested},
	AccountingPosted:       {},
}

var settlementTransitions = map[SettlementState][]SettlementState{
	SettlementUnsettled:        {SettlementPartiallySettled, SettlementSettled},
	SettlementPartiallySettled: {SettlementPartiallySettled, SettlementSettled},
	SettlementSettled:          {},
}

var holdTransitions = map[HoldState][]HoldState{
	HoldNone:     {HoldHeld, HoldDisputed},
	HoldHeld:     {HoldNone, HoldDisputed},
	HoldDisputed: {HoldNone, HoldHeld},
}

func transitionAllowed[T comparable](table map[T][]T, from, to T) bool {
	for _, t := range table[from] {
		if t == to {
			return true
		}
	}
	return false
}

func illegal(dim string, from, to any) error {
	return fmt.Errorf("%w: %s %v -> %v", ErrInvalidTransition, dim, from, to)
}

func (v *VendorInvoice) SetIntake(to IntakeState) error {
	if !transitionAllowed(intakeTransitions, v.IntakeState, to) {
		return illegal("intake", v.IntakeState, to)
	}
	v.IntakeState = to
	return nil
}

func (v *VendorInvoice) SetMatch(to MatchState) error {
	if !transitionAllowed(matchTransitions, v.MatchState, to) {
		return illegal("match", v.MatchState, to)
	}
	v.MatchState = to
	return nil
}

func (v *VendorInvoice) SetApproval(to ApprovalState) error {
	if !transitionAllowed(approvalTransitions, v.ApprovalState, to) {
		return illegal("approval", v.ApprovalState, to)
	}
	v.ApprovalState = to
	return nil
}

func (v *VendorInvoice) SetAccounting(to AccountingState) error {
	if !transitionAllowed(accountingTransitions, v.AccountingState, to) {
		return illegal("accounting", v.AccountingState, to)
	}
	v.AccountingState = to
	return nil
}

func (v *VendorInvoice) SetSettlement(to SettlementState) error {
	if !transitionAllowed(settlementTransitions, v.SettlementState, to) {
		return illegal("settlement", v.SettlementState, to)
	}
	// Cross-dimension rule: nothing settles before it is approved and handed to
	// accounting.
	if v.ApprovalState != ApprovalApproved || v.AccountingState == AccountingNotRequested {
		return illegal("settlement (invoice not approved/accounted)", v.SettlementState, to)
	}
	v.SettlementState = to
	return nil
}

func (v *VendorInvoice) SetHold(to HoldState) error {
	if !transitionAllowed(holdTransitions, v.HoldState, to) {
		return illegal("hold", v.HoldState, to)
	}
	v.HoldState = to
	return nil
}

// InvoiceStateDimensions is the GetInvoiceStateDimensions read model.
type InvoiceStateDimensions struct {
	InvoiceID  string          `json:"invoice_id"`
	Version    int             `json:"version"`
	Intake     IntakeState     `json:"intake"`
	Match      MatchState      `json:"match"`
	Approval   ApprovalState   `json:"approval"`
	Accounting AccountingState `json:"accounting"`
	Settlement SettlementState `json:"settlement"`
	Hold       HoldState       `json:"hold"`
	Duplicate  DuplicateState  `json:"duplicate"`
	Tax        TaxState        `json:"tax"`
	Payee      PayeeState      `json:"payee"`
	Status     InvoiceStatus   `json:"status"` // derived legacy summary
}

func (v *VendorInvoice) StateDimensions() InvoiceStateDimensions {
	return InvoiceStateDimensions{
		InvoiceID: v.InvoiceID, Version: v.Version,
		Intake: v.IntakeState, Match: v.MatchState, Approval: v.ApprovalState,
		Accounting: v.AccountingState, Settlement: v.SettlementState, Hold: v.HoldState,
		Duplicate: v.DuplicateState, Tax: v.TaxState, Payee: v.PayeeState,
		Status: DeriveStatus(v),
	}
}

// DeriveStatus computes the legacy single `status` from the dimensions. It is a
// read-side summary kept so existing consumers keep working; PAYMENT_REQUESTED
// is only ever present on rows written before the dimensions existed.
func DeriveStatus(v *VendorInvoice) InvoiceStatus {
	switch {
	case v.IntakeState == IntakeRejected || v.ApprovalState == ApprovalRejected:
		return InvoiceStatusRejected
	case v.IntakeState == IntakeQuarantined:
		return InvoiceStatusQuarantined
	case v.ApprovalState == ApprovalApproved && v.PaymentRequestedAt != nil:
		// Set only by the legacy POST .../request-payment endpoint, whose contract
		// (status and idempotent replay) is kept.
		return InvoiceStatusPaymentRequested
	case v.ApprovalState == ApprovalApproved:
		return InvoiceStatusApproved
	case v.IntakeState == IntakeValidated:
		return InvoiceStatusValidated
	default:
		return InvoiceStatusReceived
	}
}

// ── approval gate ────────────────────────────────────────────────────────────

// ApprovalBlocker names why an invoice cannot be approved, or "" when nothing
// blocks it. Code is the stable machine-readable error code.
type ApprovalBlocker struct {
	Code    string
	Error   string
	Message string
}

// ApprovalBlockers evaluates every approval pre-condition that is derivable from
// the invoice's own state (SoD and live TAX re-verification are checked by the
// caller). The first blocker wins; the order is the order a human would fix them.
func (v *VendorInvoice) ApprovalBlocker() *ApprovalBlocker {
	switch {
	case v.IntakeState == IntakeQuarantined:
		return &ApprovalBlocker{CodeDuplicateRisk, "invoice_quarantined", "a quarantined invoice cannot be approved until the quarantine is resolved"}
	case v.IntakeState != IntakeValidated:
		return &ApprovalBlocker{CodeValidationFailed, "invalid_transition", "only a validated invoice can be approved"}
	case v.DuplicateState == DuplicateSuspected:
		return &ApprovalBlocker{CodeDuplicateRisk, "duplicate_unresolved", "an unresolved duplicate assessment blocks approval"}
	case v.HoldState != HoldNone:
		return &ApprovalBlocker{CodeHeldOrDisputed, "invoice_held_or_disputed", "a held or disputed invoice cannot be approved"}
	case v.PayeeState == PayeeMismatch:
		return &ApprovalBlocker{CodePayeeVersionMismatch, "payee_mismatch_unresolved", "invoice bank details differ from the ORG-10 active destination; resolve the payee mismatch first"}
	case v.TaxState != TaxVerified:
		return &ApprovalBlocker{CodeTaxUnavailable, "tax_not_verified", "TAX/withholding provenance is not verified"}
	case v.RequiresMatch() && !v.MatchCleared:
		return &ApprovalBlocker{CodeMatchException, "match_not_cleared", "a MATCHED/WITHIN_TOLERANCE (or approved-variance) match run is required before approval"}
	}
	return nil
}

// AvailableActions lists the commands the invoice's current state permits. It is
// state-derived only; authorization is checked when a command is attempted.
func (v *VendorInvoice) AvailableActions() []string {
	var a []string
	add := func(s string) { a = append(a, s) }
	draft := v.SourceAcceptedAt == nil
	switch v.IntakeState {
	case IntakeReceived:
		if draft && v.HoldState == HoldNone {
			add("AmendInvoiceDraft")
		}
		add("ValidateSupplierInvoice")
		add("QuarantineSupplierInvoice")
		add("RejectSupplierInvoice")
	case IntakeValidated:
		if v.ApprovalState == ApprovalNone {
			add("SubmitInvoiceForApproval")
		}
		if v.TaxState == TaxChanged {
			add("ValidateSupplierInvoice")
		}
		if v.ApprovalState == ApprovalPending && v.ApprovalBlocker() == nil {
			add("ApproveSupplierInvoice")
		}
		if v.ApprovalState != ApprovalApproved {
			add("QuarantineSupplierInvoice")
			add("RejectSupplierInvoice")
		}
		if v.ApprovalState == ApprovalApproved && v.DocumentType == DocInvoice {
			add("AcceptSupplierCreditDocument")
		}
	case IntakeQuarantined:
		add("ResolveQuarantine")
		add("RejectSupplierInvoice")
	}
	if v.PayeeState == PayeeMismatch {
		add("ResolvePayeeMismatch")
	}
	if v.IntakeState != IntakeRejected {
		if v.HoldState == HoldNone {
			add("PlaceInvoiceHold")
		} else {
			add("ReleaseInvoiceHold")
		}
		add("LinkCorrection")
	}
	return a
}

// Stable machine-readable error codes (spec section 16).
const (
	CodeValidationFailed      = "VALIDATION_FAILED"
	CodeForbidden             = "FORBIDDEN"
	CodeSoDConflict           = "SOD_CONFLICT"
	CodeDuplicateRisk         = "DUPLICATE_RISK"
	CodeMatchException        = "MATCH_EXCEPTION"
	CodeStaleVersion          = "STALE_VERSION"
	CodeHeldOrDisputed        = "HELD_OR_DISPUTED"
	CodePayeeVersionMismatch  = "PAYEE_VERSION_MISMATCH"
	CodeAuthorizationInvalid  = "AUTHORIZATION_INVALIDATED"
	CodeTaxUnavailable        = "TAX_UNAVAILABLE"
	CodeTaxResultChanged      = "TAX_RESULT_CHANGED"
	CodeIdempotencyRequired   = "IDEMPOTENCY_KEY_REQUIRED"
	CodeIdempotencyReused     = "IDEMPOTENCY_KEY_REUSED"
	CodeDependencyUnavailable = "DEPENDENCY_UNAVAILABLE"
	CodeNotFound              = "NOT_FOUND"
	CodeGone                  = "GONE"
)
