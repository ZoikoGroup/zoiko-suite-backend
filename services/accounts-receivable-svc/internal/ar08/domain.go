// Package ar08 is AR-08 Cash Application (spec ZS-SVC-C-001 section 11): it allocates Banking-CONFIRMED receipts to receivable
// open items with deterministic reconciliation, preserving unapplied cash and external settlement ownership.
//
// Settlement authority is Banking. A receipt exists in AR only through RegisterConfirmedReceipt, which verifies a
// bank_transaction_id against the ReceiptSource (banking-connector-svc canonical transactions): status NORMALIZED, positive
// amount, valid currency. Amount, currency and value date come from Banking, never from the caller. AR never invents finality.
//
// Invariants enforced here and in the schema: receipt = applied + unapplied + authorized difference (exactly, also a CHECK);
// one ACTIVE application per receipt; an application line never exceeds the open item's residual (AR-07 owns the residual and
// refuses); all money moves in ONE transaction with the AR-07 adjustments, the ACC-04 posting request, outbox and audit.
package ar08

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Aggregate types (outbox / audit).
const (
	AggregateReceipt     = "ar08_receipt"
	AggregateBatch       = "ar08_batch"
	AggregateApplication = "ar08_application"
)

// Authz actions (authorization-svc action types). Reads are tenant-scoped (the settled AR read posture).
const (
	ActionReceiptRegister    = "AR08_RECEIPT_REGISTER"    // register a Banking-confirmed receipt
	ActionApplicationManage  = "AR08_APPLICATION_MANAGE"  // create batch, generate candidates, apply, propose manual matches, reconcile
	ActionManualMatchApprove = "AR08_MANUALMATCH_APPROVE" // approve a manual / high-value / related-party match (a different principal)
	ActionUnapply            = "AR08_UNAPPLY"             // restricted: reverse an application
	ActionReapply            = "AR08_REAPPLY"             // restricted: create a new application after an unapply
)

// Events (ar.cash.<verb>).
const (
	EvProposed              = "ar.cash.receipt_application_proposed"
	EvApplied               = "ar.cash.applied"
	EvUnapplied             = "ar.cash.unapplied"
	EvReconciled            = "ar.cash.application_reconciled"
	EvUnappliedByCorrection = "ar.cash.unapplied_by_correction"
)

// GL mapping keys used by the cash postings (seeded per tenant by general-ledger-svc, ACC-02).
const (
	KeyCashReceipt   = "AR_CASH_RECEIPT"
	KeyReceivable    = "AR_RECEIVABLE_STANDARD"
	KeyUnappliedCash = "AR_UNAPPLIED_CASH"
	KeyFXDifference  = "AR_FX_DIFFERENCE"
	KeyBankFee       = "AR_BANK_FEE_EXPENSE"
)

// Policy constants (matching policy / tolerance policy; package constants by design).
const (
	AutoApplyMinScore = 70 // a line auto-applies only when its candidate score is at least this
	MaxLines          = 200
	MaxBatchReceipts  = 500
)

// AutoApplyHighValueLimit: an application whose total (applied + difference) exceeds it always needs independent approval.
var AutoApplyHighValueLimit = kit.MustAmount("10000.0000")

// MaxDifference is the largest authorized fee / FX difference of one application (tolerance policy).
var MaxDifference = kit.MustAmount("50.0000")

// CodeManualApprovalRequired is returned when an application needs approval by a different principal.
const CodeManualApprovalRequired = "MANUAL_APPROVAL_REQUIRED"

// Receipt / batch / application / match states.
const (
	RcptUnapplied = "UNAPPLIED"
	RcptPartial   = "PARTIALLY_APPLIED"
	RcptFull      = "FULLY_APPLIED"

	BatchUnapplied  = "UNAPPLIED"
	BatchProposed   = "PROPOSED"
	BatchPartial    = "PARTIALLY_APPLIED"
	BatchFull       = "FULLY_APPLIED"
	BatchReconciled = "RECONCILED"

	AppApplied   = "APPLIED"
	AppUnapplied = "UNAPPLIED"

	ModeAuto    = "AUTO"
	ModeManual  = "MANUAL"
	ModeReapply = "REAPPLY"

	MatchPending  = "PENDING"
	MatchApplied  = "APPLIED"
	MatchRejected = "REJECTED"

	DiffNone    = ""
	DiffBankFee = "BANK_FEE"
	DiffFX      = "FX_DIFFERENCE"
)

// Banking canonical transaction statuses (banking-connector-svc).
const (
	BankNormalized  = "NORMALIZED"
	BankQuarantined = "QUARANTINED"
	BankSuperseded  = "SUPERSEDED"
)

// ── error constructors ───────────────────────────────────────────────────────

func errNotConfirmed(msg string) *kit.Problem {
	return kit.Err(http.StatusUnprocessableEntity, kit.CodeSettlementNotConfirmed, msg)
}
func errSettlementUnknown() *kit.Problem {
	return kit.Err(http.StatusServiceUnavailable, kit.CodeSettlementNotConfirmed, "Banking cannot confirm the settlement right now; nothing was created or applied")
}
func errDuplicate(msg string) *kit.Problem {
	return kit.Conflict(kit.CodeCashApplicationDuplicate, msg)
}
func errOutOfBalance(msg string) *kit.Problem {
	return kit.Unprocessable(kit.CodeApplicationOutOfBalance, msg)
}
func errSoD(msg string) *kit.Problem { return kit.Forbidden(kit.CodeSegregationOfDuties, msg) }
func errScope(msg string) *kit.Problem {
	return kit.Forbidden(kit.CodeTenantScope, msg)
}
func errApprovalRequired(msg string) *kit.Problem {
	return kit.Unprocessable(CodeManualApprovalRequired, msg)
}
func errTransition(msg string) *kit.Problem { return kit.Conflict(kit.CodeInvalidTransition, msg) }
func errCurrency() *kit.Problem {
	return kit.Unprocessable(kit.CodeValidationFailed, "cross-currency cash application is not implemented: the receipt and every open item must share one currency")
}

func mustUUID(field, v string) error {
	if _, err := uuid.Parse(v); err != nil {
		return kit.Invalid(field + " must be a UUID")
	}
	return nil
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ── aggregates ───────────────────────────────────────────────────────────────

// Receipt is a Banking-confirmed receipt: receipt = applied + unapplied + difference.
type Receipt struct {
	ReceiptID         string     `json:"receipt_id"`
	TenantID          string     `json:"-"`
	LegalEntityID     string     `json:"legal_entity_id"`
	BankTransactionID string     `json:"bank_transaction_id"`
	SettlementRef     string     `json:"settlement_ref"`
	CustomerPartyID   string     `json:"customer_party_id,omitempty"`
	Currency          string     `json:"currency"`
	Amount            kit.Amount `json:"amount"`
	AppliedAmount     kit.Amount `json:"applied_amount"`
	UnappliedAmount   kit.Amount `json:"unapplied_amount"`
	DifferenceAmount  kit.Amount `json:"difference_amount"`
	ValueDate         string     `json:"value_date"`
	PayerText         string     `json:"payer_text"`
	RemittanceText    string     `json:"remittance_text"`
	RelatedParty      bool       `json:"related_party"`
	State             string     `json:"state"`
	Blocked           bool       `json:"blocked"`
	ControlException  string     `json:"control_exception,omitempty"`
	RegisteredBy      string     `json:"registered_by"`
	Version           int        `json:"version"`
}

// Batch is an application batch (one currency).
type Batch struct {
	BatchID       string   `json:"batch_id"`
	LegalEntityID string   `json:"legal_entity_id"`
	Currency      string   `json:"currency"`
	Reference     string   `json:"reference"`
	State         string   `json:"state"`
	Version       int      `json:"version"`
	CreatedBy     string   `json:"created_by"`
	ReceiptIDs    []string `json:"receipt_ids,omitempty"`
}

// Line is one allocation of an application.
type Line struct {
	LineNo     int        `json:"line_no,omitempty"`
	OpenItemID string     `json:"open_item_id"`
	Amount     kit.Amount `json:"amount"`
}

// Application is one allocation of one receipt.
type Application struct {
	ApplicationID    string     `json:"application_id"`
	LegalEntityID    string     `json:"legal_entity_id"`
	ReceiptID        string     `json:"receipt_id"`
	BatchID          string     `json:"batch_id"`
	Status           string     `json:"status"`
	Mode             string     `json:"mode"`
	RequestHash      string     `json:"-"`
	Currency         string     `json:"currency"`
	ReceiptAmount    kit.Amount `json:"receipt_amount"`
	AppliedAmount    kit.Amount `json:"applied_amount"`
	UnappliedAmount  kit.Amount `json:"unapplied_amount"`
	DifferenceAmount kit.Amount `json:"difference_amount"`
	DifferenceKind   string     `json:"difference_kind,omitempty"`
	ReasonCode       string     `json:"reason_code,omitempty"`
	EvidenceID       string     `json:"evidence_id,omitempty"`
	MatchID          string     `json:"match_id,omitempty"`
	Reapplies        string     `json:"reapplies_application_id,omitempty"`
	AppliedBy        string     `json:"applied_by"`
	ApprovedBy       string     `json:"approved_by,omitempty"`
	AppliedAt        time.Time  `json:"applied_at"`
	UnappliedBy      string     `json:"unapplied_by,omitempty"`
	UnappliedAt      *time.Time `json:"unapplied_at,omitempty"`
	UnapplyReason    string     `json:"unapply_reason_code,omitempty"`
	AccountingState  string     `json:"accounting_state"`
	JournalID        string     `json:"journal_id,omitempty"`
	AccountingError  string     `json:"accounting_error,omitempty"`
	UnapplyState     string     `json:"unapply_accounting_state,omitempty"`
	UnapplyJournalID string     `json:"unapply_journal_id,omitempty"`
	Version          int        `json:"version"`
	Lines            []Line     `json:"lines"`
}

// Candidate is a persisted match candidate with its score evidence.
type Candidate struct {
	CandidateID     string     `json:"candidate_id,omitempty"`
	Generation      int        `json:"generation,omitempty"`
	Rank            int        `json:"rank"`
	OpenItemID      string     `json:"open_item_id"`
	CustomerPartyID string     `json:"customer_party_id"`
	Score           int        `json:"score"`
	Basis           []string   `json:"basis"`
	Residual        kit.Amount `json:"residual_amount"`
	Suggested       kit.Amount `json:"suggested_amount"`
	Disputed        bool       `json:"disputed"`
	CollectionHold  bool       `json:"collection_hold"`
	DueDate         time.Time  `json:"-"`
}

// Eligible is whether the candidate may be auto-applied.
func (c Candidate) Eligible() bool {
	return c.Score >= AutoApplyMinScore && !c.Disputed && !c.CollectionHold && c.Residual.IsPositive()
}

// ManualMatch is a proposed allocation awaiting a different approver.
type ManualMatch struct {
	MatchID          string     `json:"match_id"`
	ReceiptID        string     `json:"receipt_id"`
	BatchID          string     `json:"batch_id"`
	Lines            []Line     `json:"lines"`
	DifferenceAmount kit.Amount `json:"difference_amount"`
	DifferenceKind   string     `json:"difference_kind,omitempty"`
	ReasonCode       string     `json:"reason_code"`
	EvidenceID       string     `json:"evidence_id,omitempty"`
	Requires         string     `json:"requires,omitempty"`
	Status           string     `json:"status"`
	ProposedBy       string     `json:"proposed_by"`
	ApprovedBy       string     `json:"approved_by,omitempty"`
	ApplicationID    string     `json:"application_id,omitempty"`
}

// ── scoring (deterministic) ──────────────────────────────────────────────────

// Score weights.
const (
	scoreReference = 60
	scoreAmount    = 30
	scoreCustomer  = 10
)

var ageBonus = []int{5, 3, 1}

// Basis labels persisted as evidence.
const (
	BasisReference = "REFERENCE_MATCH"
	BasisAmount    = "AMOUNT_EQUALS_RESIDUAL"
	BasisCustomer  = "CUSTOMER_FROM_RECEIPT"
	BasisInferred  = "CUSTOMER_INFERRED_FROM_PAYER_TEXT"
	BasisAge       = "DUE_DATE_ORDER"
)

// InferCustomer returns the first UUID token in the payer / remittance text (a customer reference the payer quoted), or "".
func InferCustomer(texts ...string) string {
	for _, t := range texts {
		for _, tok := range strings.FieldsFunc(t, func(r rune) bool {
			return !(r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'))
		}) {
			tok = strings.Trim(tok, "-")
			if len(tok) == 36 {
				if u, err := uuid.Parse(tok); err == nil {
					return strings.ToLower(u.String())
				}
			}
		}
	}
	return ""
}

// ScoreCandidates scores open items for a receipt. customerBasis is BasisCustomer or BasisInferred. The result is ordered
// score desc, due date asc, open item id asc, and ranks are assigned 1..n. Same inputs always give the same output.
func ScoreCandidates(r *Receipt, items []kit.OpenItemView, customerBasis string) []Candidate {
	its := append([]kit.OpenItemView(nil), items...)
	sort.Slice(its, func(i, j int) bool {
		if !its[i].DueDate.Equal(its[j].DueDate) {
			return its[i].DueDate.Before(its[j].DueDate)
		}
		return its[i].OpenItemID < its[j].OpenItemID
	})
	text := strings.ToLower(r.PayerText + " " + r.RemittanceText)
	out := make([]Candidate, 0, len(its))
	for idx, it := range its {
		if !it.Residual.IsPositive() || it.Currency != r.Currency {
			continue
		}
		c := Candidate{OpenItemID: it.OpenItemID, CustomerPartyID: it.CustomerPartyID, Residual: it.Residual, Disputed: it.Disputed,
			CollectionHold: it.CollectionHold, DueDate: it.DueDate, Basis: []string{}}
		if customerBasis != "" {
			c.Score += scoreCustomer
			c.Basis = append(c.Basis, customerBasis)
		}
		if (it.SourceDocumentID != "" && strings.Contains(text, strings.ToLower(it.SourceDocumentID))) ||
			(it.OpenItemID != "" && strings.Contains(text, strings.ToLower(it.OpenItemID))) {
			c.Score += scoreReference
			c.Basis = append(c.Basis, BasisReference)
		}
		if it.Residual == r.UnappliedAmount || it.Residual == r.Amount {
			c.Score += scoreAmount
			c.Basis = append(c.Basis, BasisAmount)
		}
		if idx < len(ageBonus) {
			c.Score += ageBonus[idx]
			c.Basis = append(c.Basis, BasisAge)
		}
		if c.Score > 100 {
			c.Score = 100
		}
		c.Suggested = kit.MinAmount(it.Residual, r.Amount)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if !out[i].DueDate.Equal(out[j].DueDate) {
			return out[i].DueDate.Before(out[j].DueDate)
		}
		return out[i].OpenItemID < out[j].OpenItemID
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

// AllocateAuto allocates the receipt over auto-eligible candidates in rank order, each capped at its residual. Whatever
// cannot be allocated stays unapplied. Lines are returned sorted by open item id (the lock order).
func AllocateAuto(amount kit.Amount, cands []Candidate) []Line {
	left := amount
	var lines []Line
	for _, c := range cands {
		if left.IsZero() {
			break
		}
		if !c.Eligible() {
			continue
		}
		a := kit.MinAmount(c.Residual, left)
		lines = append(lines, Line{OpenItemID: c.OpenItemID, Amount: a})
		left = left.Sub(a)
	}
	SortLines(lines)
	return lines
}

// SortLines orders lines by open item id and numbers them: deterministic ordering and a global lock order.
func SortLines(lines []Line) {
	sort.Slice(lines, func(i, j int) bool { return lines[i].OpenItemID < lines[j].OpenItemID })
	for i := range lines {
		lines[i].LineNo = i + 1
	}
}

// ── validation + arithmetic ──────────────────────────────────────────────────

// Split is the three-way split of a receipt.
type Split struct {
	Applied, Unapplied, Difference kit.Amount
}

// ValidateLines checks the allocation lines (positive, currency-exact, unique items, bounded) and the difference, and
// returns the split that makes receipt = applied + unapplied + difference EXACTLY (APPLICATION_OUT_OF_BALANCE otherwise).
// declaredUnapplied, when non-nil, is the caller's own figure and must equal the computed remainder.
func ValidateLines(r *Receipt, lines []Line, diff kit.Amount, diffKind string, declaredUnapplied *kit.Amount) (Split, error) {
	if len(lines) == 0 {
		return Split{}, kit.Invalid("at least one application line is required")
	}
	if len(lines) > MaxLines {
		return Split{}, kit.Invalid(fmt.Sprintf("at most %d lines per application", MaxLines))
	}
	seen := map[string]bool{}
	var applied kit.Amount
	for i, l := range lines {
		if err := mustUUID(fmt.Sprintf("lines[%d].open_item_id", i), l.OpenItemID); err != nil {
			return Split{}, err
		}
		if seen[l.OpenItemID] {
			return Split{}, kit.Invalid("an open item may appear once per application")
		}
		seen[l.OpenItemID] = true
		if !l.Amount.IsPositive() {
			return Split{}, kit.Invalid("line amounts must be positive")
		}
		if !l.Amount.FitsCurrency(r.Currency) {
			return Split{}, kit.Invalid("a line amount has more decimals than the currency allows")
		}
		applied = applied.Add(l.Amount)
	}
	if diff.IsNegative() || !diff.FitsCurrency(r.Currency) {
		return Split{}, kit.Invalid("difference_amount must be a non-negative amount that fits the currency")
	}
	switch {
	case diff.IsZero() && diffKind != DiffNone:
		return Split{}, kit.Invalid("difference_kind requires a difference_amount")
	case diff.IsPositive() && diffKind != DiffBankFee && diffKind != DiffFX:
		return Split{}, kit.Invalid("difference_kind must be BANK_FEE or FX_DIFFERENCE")
	case diff > MaxDifference:
		return Split{}, errOutOfBalance("the difference exceeds the tolerance policy (" + MaxDifference.String() + ")")
	}
	total := applied.Add(diff)
	if total > r.Amount {
		return Split{}, errOutOfBalance("applied + difference " + total.String() + " exceeds the receipt " + r.Amount.String())
	}
	s := Split{Applied: applied, Difference: diff, Unapplied: r.Amount.Sub(total)}
	if declaredUnapplied != nil && *declaredUnapplied != s.Unapplied {
		return Split{}, errOutOfBalance("receipt " + r.Amount.String() + " does not equal applied " + applied.String() + " + unapplied " + declaredUnapplied.String() + " + difference " + diff.String())
	}
	return s, nil
}

// ReceiptState is the receipt state implied by its split.
func ReceiptState(s Split) string {
	switch {
	case s.Applied.IsZero():
		return RcptUnapplied
	case s.Unapplied.IsZero():
		return RcptFull
	default:
		return RcptPartial
	}
}

// BatchStateOf derives the batch state from its receipts (RECONCILED is set only by ReconcileApplicationBatch).
func BatchStateOf(receipts, applied, full int, pendingProposal bool) string {
	switch {
	case receipts > 0 && applied == receipts && full == receipts:
		return BatchFull
	case applied > 0:
		return BatchPartial
	case pendingProposal:
		return BatchProposed
	default:
		return BatchUnapplied
	}
}

// RequestHash binds an application's content so a retry with another key is recognised as the same request.
func RequestHash(batchID, receiptID, mode string, lines []Line, diff kit.Amount, diffKind string) string {
	ls := append([]Line(nil), lines...)
	sort.Slice(ls, func(i, j int) bool { return ls[i].OpenItemID < ls[j].OpenItemID })
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%s|%s", batchID, receiptID, mode, diff, diffKind)
	for _, l := range ls {
		fmt.Fprintf(&b, "|%s=%s", l.OpenItemID, l.Amount)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// ApprovalRequirement says why an application cannot execute on one principal's authority ("" when it can).
func ApprovalRequirement(r *Receipt, mode string, applied, diff kit.Amount, cands []Candidate, lines []Line) string {
	switch {
	case mode != ModeAuto:
		return "MANUAL_MATCH"
	case r.RelatedParty:
		return "RELATED_PARTY"
	case diff.IsPositive():
		return "AUTHORIZED_DIFFERENCE"
	case applied.Add(diff) > AutoApplyHighValueLimit:
		return "HIGH_VALUE"
	}
	byItem := map[string]Candidate{}
	for _, c := range cands {
		byItem[c.OpenItemID] = c
	}
	for _, l := range lines {
		c, ok := byItem[l.OpenItemID]
		if !ok || !c.Eligible() {
			return "LOW_CONFIDENCE_OR_DISPUTED"
		}
	}
	return ""
}

// ── accounting ───────────────────────────────────────────────────────────────

// BuildPosting is the documented posting scheme (spec section 17, "Cash applied"):
//
//	apply:   Dr AR_CASH_RECEIPT (receipt amount) / Cr AR_RECEIVABLE_STANDARD (applied) / Cr AR_UNAPPLIED_CASH (unapplied
//	         remainder, when > 0) / Cr AR_BANK_FEE_EXPENSE or AR_FX_DIFFERENCE (authorized difference, when > 0)
//	unapply: the exact mirror image (sides swapped) under a distinct source_event_id.
//
// receipt = applied + unapplied + difference keeps every event balanced in exact arithmetic.
func BuildPosting(a *Application, reverse bool) []kit.PostingLine {
	cr := func(key string, amt kit.Amount, d string) kit.PostingLine {
		if reverse {
			return kit.PostingLine{MappingKey: key, Debit: amt, Description: d}
		}
		return kit.PostingLine{MappingKey: key, Credit: amt, Description: d}
	}
	dr := func(key string, amt kit.Amount, d string) kit.PostingLine {
		if reverse {
			return kit.PostingLine{MappingKey: key, Credit: amt, Description: d}
		}
		return kit.PostingLine{MappingKey: key, Debit: amt, Description: d}
	}
	lines := []kit.PostingLine{dr(KeyCashReceipt, a.ReceiptAmount, "Cash receipt"), cr(KeyReceivable, a.AppliedAmount, "Applied to receivables")}
	if a.UnappliedAmount.IsPositive() {
		lines = append(lines, cr(KeyUnappliedCash, a.UnappliedAmount, "Unapplied cash"))
	}
	if a.DifferenceAmount.IsPositive() {
		key := KeyBankFee
		if a.DifferenceKind == DiffFX {
			key = KeyFXDifference
		}
		lines = append(lines, cr(key, a.DifferenceAmount, "Authorized "+strings.ToLower(a.DifferenceKind)))
	}
	return lines
}
