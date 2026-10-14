// Package ar07 implements AR-07 Receivables (spec ZS-SVC-C-001 section 10): customer open items, their residuals,
// the append-only adjustment ledger and the derived aging / balance projections. A residual moves ONLY through an
// adjustment that names its owning event or document (invariants 6, 7, 15, 17; section 18 #19, #20, #21). There is no
// code path, endpoint or SQL statement that sets a balance directly.
package ar07

import (
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Authorization actions. Reads are tenant-scoped only (settled AR read posture).
const (
	ActionReceivableManage = "AR07_RECEIVABLE_MANAGE" // receivables.manage: HTTP twin of CreateOpenItemFromDocument, CloseOpenItem
	ActionReceivableAdjust = "AR07_RECEIVABLE_ADJUST" // privileged: the only HTTP adjustment (kind CORRECTION)
	ActionDisputeManage    = "AR07_DISPUTE_MANAGE"    // receivables.dispute: OpenDispute, ResolveDispute
	ActionHoldManage       = "AR07_HOLD_MANAGE"       // receivables.hold: PlaceCollectionHold, ReleaseCollectionHold
)

// Actions lists every AR-07 authorization action.
var Actions = []string{ActionReceivableManage, ActionReceivableAdjust, ActionDisputeManage, ActionHoldManage}

// Events (kit.Emit EventType).
const (
	EvOpenItemCreated   = "ar.receivable.open_item_created"
	EvPartiallySettled  = "ar.receivable.partially_settled"
	EvSettled           = "ar.receivable.settled"
	EvReopened          = "ar.receivable.reopened"
	EvDisputed          = "ar.receivable.disputed"
	EvDisputeResolved   = "ar.receivable.dispute_resolved"
	EvHoldPlaced        = "ar.receivable.collection_hold_placed"
	EvHoldReleased      = "ar.receivable.collection_hold_released"
	EvWrittenOff        = "ar.receivable.written_off"
	EvClosed            = "ar.receivable.closed"
	AggregateOpenItem   = "AR_OPEN_ITEM"
	StateCreated        = "CREATED"
	StateOpen           = "OPEN"
	StatePartiallySettl = "PARTIALLY_SETTLED"
	StateSettled        = "SETTLED"
	StateWrittenOff     = "WRITTEN_OFF"
	StateCorrected      = "CORRECTED"
	DefaultCalendar     = "GREGORIAN_UTC"
)

// Resolution codes of a dispute.
const (
	ResolutionUpheld   = "UPHELD"   // the customer was right: a credit / correction follows through its own document
	ResolutionRejected = "REJECTED" // the dispute is withdrawn or rejected: the item is collectable again
)

// OpenItem is the receivable. Residual is derived from the ledger and only ever written by an adjustment.
type OpenItem struct {
	OpenItemID         string     `json:"open_item_id"`
	TenantID           string     `json:"tenant_id"`
	LegalEntityID      string     `json:"legal_entity_id"`
	DocumentKind       string     `json:"document_kind"`
	SourceDocumentID   string     `json:"source_document_id"`
	DocumentNumber     string     `json:"document_number"`
	CustomerPartyID    string     `json:"customer_party_id"`
	Currency           string     `json:"currency"`
	OriginalAmount     kit.Amount `json:"original_amount"`
	Residual           kit.Amount `json:"residual_amount"`
	IssueDate          string     `json:"issue_date"`
	DueDate            string     `json:"due_date"`
	BusinessCalendarID string     `json:"business_calendar_id"`
	State              string     `json:"state"`
	Disputed           bool       `json:"disputed"`
	CollectionHold     bool       `json:"collection_hold"`
	HoldReason         string     `json:"hold_reason,omitempty"`
	ClosedAt           *time.Time `json:"closed_at,omitempty"`
	Version            int        `json:"version"`
	CreatedBy          string     `json:"created_by"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// View projects the open item onto the cross-module port type.
func (o *OpenItem) View() *kit.OpenItemView {
	due, _ := time.Parse("2006-01-02", o.DueDate)
	return &kit.OpenItemView{OpenItemID: o.OpenItemID, DocumentKind: o.DocumentKind, SourceDocumentID: o.SourceDocumentID,
		CustomerPartyID: o.CustomerPartyID, LegalEntityID: o.LegalEntityID, Currency: o.Currency, OriginalAmount: o.OriginalAmount,
		Residual: o.Residual, DueDate: due, State: o.State, Disputed: o.Disputed, CollectionHold: o.CollectionHold, Version: o.Version}
}

// Adjustment is one row of the append-only ledger.
type Adjustment struct {
	AdjustmentID  string     `json:"adjustment_id"`
	Seq           int64      `json:"seq"`
	OpenItemID    string     `json:"open_item_id"`
	Kind          string     `json:"kind"`
	Amount        kit.Amount `json:"amount"`
	SignedEffect  kit.Amount `json:"signed_effect"`
	BalanceBefore kit.Amount `json:"balance_before"`
	BalanceAfter  kit.Amount `json:"balance_after"`
	SourceRefType string     `json:"source_ref_type"`
	SourceRefID   string     `json:"source_ref_id"`
	EventKey      string     `json:"event_key"`
	Reason        string     `json:"reason,omitempty"`
	ReasonCode    string     `json:"reason_code,omitempty"`
	EvidenceID    string     `json:"evidence_id,omitempty"`
	ActorID       string     `json:"actor_id"`
	VersionAfter  int        `json:"version_after"`
	RecordedAt    time.Time  `json:"recorded_at"`
}

// HistoryEvent is one row of the append-only item history.
type HistoryEvent struct {
	Seq           int64          `json:"seq"`
	Event         string         `json:"event"`
	FromState     string         `json:"from_state,omitempty"`
	ToState       string         `json:"to_state,omitempty"`
	ResidualAfter kit.Amount     `json:"residual_after"`
	VersionAfter  int            `json:"version_after"`
	ActorID       string         `json:"actor_id"`
	ReasonCode    string         `json:"reason_code,omitempty"`
	EvidenceID    string         `json:"evidence_id,omitempty"`
	Detail        map[string]any `json:"detail,omitempty"`
	At            time.Time      `json:"at"`
}

// Dispute is one dispute of an open item.
type Dispute struct {
	DisputeID            string     `json:"dispute_id"`
	Status               string     `json:"status"`
	ReasonCode           string     `json:"reason_code"`
	EvidenceID           string     `json:"evidence_id"`
	OpenedBy             string     `json:"opened_by"`
	OpenedAt             time.Time  `json:"opened_at"`
	ResolutionCode       string     `json:"resolution_code,omitempty"`
	ResolutionEvidenceID string     `json:"resolution_evidence_id,omitempty"`
	ResolvedBy           string     `json:"resolved_by,omitempty"`
	ResolvedAt           *time.Time `json:"resolved_at,omitempty"`
}

// adjustmentSign is the direction of an adjustment kind: -1 reduces the residual, +1 raises it.
//
//	CASH_APPLICATION, CREDIT_NOTE, WRITE_OFF, CORRECTION  reduce (a correction only ever reduces)
//	CASH_UNAPPLICATION, REFUND                            raise: they reverse a previous reduction (a refund returns value
//	                                                      that a credit or cash had cleared); never above the original amount
//
// DEBIT_NOTE is NOT an adjustment: a debit note is its own receivable (CreateFromDocument).
func adjustmentSign(kind string) (int, bool) {
	switch kind {
	case kit.AdjCashApplication, kit.AdjCreditNote, kit.AdjWriteOff, kit.AdjCorrection:
		return -1, true
	case kit.AdjCashUnapply, kit.AdjRefund:
		return 1, true
	}
	return 0, false
}

// ApplyToResidual computes the residual after one adjustment, refusing a residual outside [0, original] (#19).
func ApplyToResidual(residual, original kit.Amount, kind string, amount kit.Amount) (kit.Amount, error) {
	sign, ok := adjustmentSign(kind)
	if !ok {
		if kind == kit.AdjDebitNote {
			return 0, kit.Invalid("a debit note is its own receivable (create an open item from it), not an adjustment")
		}
		return 0, kit.Invalid("adjustment kind is not valid")
	}
	if !amount.IsPositive() {
		return 0, kit.Invalid("adjustment amount must be a positive magnitude")
	}
	next := residual.Add(amount)
	if sign < 0 {
		next = residual.Sub(amount)
	}
	if next.IsNegative() {
		return 0, kit.Unprocessable(kit.CodeReceivableResidualInvalid, "the adjustment exceeds the residual; a residual cannot become negative without an authorized credit")
	}
	if next > original {
		return 0, kit.Unprocessable(kit.CodeReceivableResidualInvalid, "the adjustment would raise the residual above the original amount")
	}
	return next, nil
}

// DeriveState is the state implied by a residual and the kind of the adjustment that produced it.
func DeriveState(residual, original kit.Amount, lastKind string) string {
	switch {
	case residual == original:
		return StateOpen
	case residual.IsPositive():
		return StatePartiallySettl
	case lastKind == kit.AdjWriteOff:
		return StateWrittenOff
	case lastKind == kit.AdjCorrection:
		return StateCorrected
	}
	return StateSettled
}

// SettlementDimension maps a residual onto the AR-04 settlement dimension.
func SettlementDimension(residual, original kit.Amount, lastKind string) string {
	switch {
	case residual == original:
		return "UNPAID"
	case residual.IsPositive():
		return "PARTIALLY_PAID"
	case lastKind == kit.AdjWriteOff:
		return "WRITTEN_OFF"
	case lastKind == kit.AdjCreditNote, lastKind == kit.AdjCorrection:
		return "CREDITED"
	}
	return "PAID"
}

// IsTerminal: written-off and corrected items accept no further adjustment.
func IsTerminal(state string) bool { return state == StateWrittenOff || state == StateCorrected }

// IsLive reports an item that can still be disputed or held: it has a residual and is neither terminal nor closed.
func (o *OpenItem) IsLive() bool {
	return o.ClosedAt == nil && o.Residual.IsPositive() && !IsTerminal(o.State)
}

var (
	currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)
	refTypeRE  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
)

func mustUUID(field, v string) error {
	if _, err := uuid.Parse(v); err != nil {
		return kit.Invalid(field + " must be a UUID")
	}
	return nil
}

func parseDate(field, s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, kit.Invalid(field + " must be YYYY-MM-DD")
	}
	return t, nil
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// ValidateCreate checks a document-to-open-item request (pure).
func ValidateCreate(in kit.OpenItemFromDocument) error {
	switch {
	case in.DocumentKind == kit.DocCreditNote:
		return kit.Invalid("a credit note is applied as an adjustment to its source open item, not opened as a receivable")
	case in.DocumentKind != kit.DocInvoice && in.DocumentKind != kit.DocDebitNote:
		return kit.Invalid("document_kind must be INVOICE or DEBIT_NOTE")
	case !currencyRE.MatchString(in.Currency):
		return kit.Invalid("currency must be a 3-letter upper-case code")
	case !in.OriginalAmount.IsPositive():
		return kit.Invalid("original_amount must be positive")
	case !in.OriginalAmount.FitsCurrency(in.Currency):
		return kit.Invalid("original_amount has more decimals than the currency allows")
	case len(strings.TrimSpace(in.DocumentNumber)) > 64:
		return kit.Invalid("document_number is at most 64 characters")
	case in.IssueDate.IsZero() || in.DueDate.IsZero():
		return kit.Invalid("issue_date and due_date are required")
	case day(in.DueDate) < day(in.IssueDate):
		return kit.Invalid("due_date cannot precede issue_date")
	}
	if err := mustUUID("source_document_id", in.SourceDocumentID); err != nil {
		return err
	}
	return mustUUID("customer_party_id", in.CustomerPartyID)
}

// ValidateAdjustment checks the shape of an adjustment request (pure); the residual rule needs the item.
func ValidateAdjustment(in kit.OpenItemAdjustment) error {
	if _, ok := adjustmentSign(in.Kind); !ok {
		if in.Kind == kit.AdjDebitNote {
			return kit.Invalid("a debit note is its own receivable (create an open item from it), not an adjustment")
		}
		return kit.Invalid("adjustment kind is not valid")
	}
	switch {
	case !in.Amount.IsPositive():
		return kit.Invalid("adjustment amount must be a positive magnitude")
	case !refTypeRE.MatchString(in.SourceRefType):
		return kit.Invalid("an adjustment must name its owning event or document type (source_ref_type, UPPER_SNAKE)")
	case strings.TrimSpace(in.SourceRefID) == "" || len(in.SourceRefID) > 128:
		return kit.Invalid("an adjustment must name its owning event or document id (source_ref_id, max 128 characters)")
	case strings.TrimSpace(in.EventKey) == "" || len(in.EventKey) > 255:
		return kit.Invalid("event_key is required (max 255 characters)")
	case len(in.Reason) > 500:
		return kit.Invalid("reason is at most 500 characters")
	}
	return mustUUID("open_item_id", in.OpenItemID)
}

// AgingBuckets in report order.
var AgingBuckets = []string{"current", "1-30", "31-60", "61-90", "90+"}

// AgingBucket places days past due (as_of - due_date) into its bucket: <= 0 current, then 1-30, 31-60, 61-90, 90+.
func AgingBucket(daysPastDue int) string {
	switch {
	case daysPastDue <= 0:
		return "current"
	case daysPastDue <= 30:
		return "1-30"
	case daysPastDue <= 60:
		return "31-60"
	case daysPastDue <= 90:
		return "61-90"
	}
	return "90+"
}
