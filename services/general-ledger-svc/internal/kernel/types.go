package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ── books (standard §4) ───────────────────────────────────────────────────────

// Book types.
const (
	BookStatutory  = "PRIMARY_STATUTORY"
	BookManagement = "MANAGEMENT"
	BookTax        = "TAX"
	BookGroup      = "GROUP_CONSOLIDATION"
	BookRegulatory = "REGULATORY"
)

// Book statuses.
const (
	BookDraft     = "DRAFT"
	BookActive    = "ACTIVE"
	BookSuspended = "SUSPENDED"
	BookRetired   = "RETIRED"
)

// BookTypes lists the controlled book types.
var BookTypes = []string{BookStatutory, BookManagement, BookTax, BookGroup, BookRegulatory}

// Book is a first-class accounting-policy context for one legal entity, not a tag on a journal (AK-INV-004).
type Book struct {
	BookID                string     `json:"book_id"`
	TenantID              string     `json:"tenant_id"`
	LegalEntityID         string     `json:"legal_entity_id"`
	BookCode              string     `json:"book_code"`
	BookType              string     `json:"book_type"`
	AccountingFrameworkID string     `json:"accounting_framework_id"`
	Jurisdiction          string     `json:"jurisdiction,omitempty"`
	FunctionalCurrency    string     `json:"functional_currency"`
	FiscalCalendarID      string     `json:"fiscal_calendar_id"`
	ChartOfAccountsID     string     `json:"chart_of_accounts_id"`
	DefaultLedgerID       string     `json:"default_ledger_id"`
	EffectiveFrom         time.Time  `json:"effective_from"`
	EffectiveTo           *time.Time `json:"effective_to,omitempty"`
	Status                string     `json:"status"`
	CreatedBy             string     `json:"created_by"`
	Version               int64      `json:"version"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

var (
	codeRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)
)

// ValidateNew checks a book to be created.
func (b *Book) ValidateNew() error {
	bad := func(f, r string) error { return exc(FamData, "BOOK_INVALID", "%s: %s", f, r).with("field", f) }
	if !codeRE.MatchString(b.BookCode) {
		return bad("book_code", "1-63 letters, digits, dot, dash or underscore")
	}
	if _, err := uuid.Parse(b.LegalEntityID); err != nil {
		return bad("legal_entity_id", "a legal entity id")
	}
	ok := false
	for _, t := range BookTypes {
		ok = ok || t == b.BookType
	}
	if !ok {
		return bad("book_type", "one of "+strings.Join(BookTypes, ", "))
	}
	for f, v := range map[string]string{"accounting_framework_id": b.AccountingFrameworkID, "fiscal_calendar_id": b.FiscalCalendarID,
		"chart_of_accounts_id": b.ChartOfAccountsID, "default_ledger_id": b.DefaultLedgerID} {
		if !codeRE.MatchString(v) {
			return bad(f, "a controlled identifier, not free text")
		}
	}
	if !jurisdictionRE.MatchString(b.Jurisdiction) {
		return bad("jurisdiction", "an ISO 3166 country code, optionally with a subdivision (DE, US-CA), or empty")
	}
	if !currencyRE.MatchString(b.FunctionalCurrency) {
		return bad("functional_currency", "an ISO 4217 code")
	}
	if b.EffectiveFrom.IsZero() {
		return bad("effective_from", "required")
	}
	if b.EffectiveTo != nil && !b.EffectiveTo.After(b.EffectiveFrom) {
		return bad("effective_to", "after effective_from")
	}
	return nil
}

// InForce reports whether the book can carry postings on the date.
func (b *Book) InForce(d time.Time) bool {
	return b.Status == BookActive && !d.Before(b.EffectiveFrom) && (b.EffectiveTo == nil || d.Before(*b.EffectiveTo))
}

// ── accounting events (standard §5) ───────────────────────────────────────────

// AccountingEvent is the canonical, immutable description of an economic event. It is not a journal entry: it carries
// facts, and the posting rules decide what they mean for each book.
type AccountingEvent struct {
	AccountingEventID    string            `json:"accounting_event_id"`
	TenantID             string            `json:"tenant_id"`
	EventType            string            `json:"event_type"`
	EventVersion         int               `json:"event_version"`
	LegalEntityID        string            `json:"legal_entity_id"`
	SourceSystem         string            `json:"source_system"`
	SourceObjectType     string            `json:"source_object_type"`
	SourceObjectID       string            `json:"source_object_id"`
	SourceEventID        string            `json:"source_event_id"`
	OccurredAt           time.Time         `json:"occurred_at"`
	EffectiveAt          *time.Time        `json:"effective_at,omitempty"`
	DocumentDate         *time.Time        `json:"document_date,omitempty"`
	PostingDateRequested *time.Time        `json:"posting_date_requested,omitempty"`
	TransactionCurrency  string            `json:"transaction_currency,omitempty"`
	Facts                map[string]string `json:"facts"`
	CounterpartyID       string            `json:"counterparty_id,omitempty"`
	Dimensions           map[string]string `json:"dimensions,omitempty"`
	EvidenceRefs         []string          `json:"evidence_refs,omitempty"`
	IdempotencyKey       string            `json:"idempotency_key"`
	CorrelationID        string            `json:"correlation_id"`
	CausationID          string            `json:"causation_id"`
	// SupersedesEventID names the event this one corrects (standard §11). The original keeps its history; the correction
	// is a new economic fact with its own identity and posting.
	SupersedesEventID string    `json:"supersedes_event_id,omitempty"`
	PayloadHash       string    `json:"payload_hash"`
	RegisteredBy      string    `json:"registered_by,omitempty"`
	RegisteredAt      time.Time `json:"registered_at"`
}

var eventTypeRE = regexp.MustCompile(`^[A-Z][A-Z0-9]*(\.[A-Z][A-Z0-9_]*)+$`)

// Validate checks the event contract. A deterministic rejection names the field.
func (e *AccountingEvent) Validate() error {
	bad := func(f, r string) error { return exc(FamData, "EVENT_INVALID", "%s: %s", f, r).with("field", f) }
	if !eventTypeRE.MatchString(e.EventType) || len(e.EventType) > 80 {
		return bad("event_type", "a dotted type such as AR.INVOICE_ISSUED")
	}
	if e.EventVersion < 1 {
		return bad("event_version", "1 or more")
	}
	if _, err := uuid.Parse(e.LegalEntityID); err != nil {
		return bad("legal_entity_id", "a legal entity id")
	}
	for f, v := range map[string]string{"source_system": e.SourceSystem, "source_object_type": e.SourceObjectType, "source_object_id": e.SourceObjectID,
		"source_event_id": e.SourceEventID, "idempotency_key": e.IdempotencyKey, "correlation_id": e.CorrelationID, "causation_id": e.CausationID} {
		if strings.TrimSpace(v) == "" || len(v) > 200 {
			return bad(f, "required, at most 200 characters")
		}
	}
	if e.OccurredAt.IsZero() {
		return bad("occurred_at", "required")
	}
	if e.OccurredAt.After(time.Now().Add(24 * time.Hour)) {
		return bad("occurred_at", "not in the future")
	}
	if e.SupersedesEventID != "" {
		if _, err := uuid.Parse(e.SupersedesEventID); err != nil {
			return bad("supersedes_event_id", "an accounting event id")
		}
	}
	if e.TransactionCurrency != "" && !currencyRE.MatchString(e.TransactionCurrency) {
		return bad("transaction_currency", "an ISO 4217 code")
	}
	if len(e.Facts) > 64 || len(e.Dimensions) > 24 || len(e.EvidenceRefs) > 50 {
		return bad("facts", "too many facts, dimensions or evidence references")
	}
	for k, v := range e.Facts {
		if !identRE.MatchString(k) || len(v) > 500 {
			return bad("facts."+k, "a lower-case fact name with a value of at most 500 characters")
		}
	}
	for k, v := range e.Dimensions {
		if !codeRE.MatchString(k) || len(v) > 120 {
			return bad("dimensions."+k, "a dimension code with a value of at most 120 characters")
		}
	}
	if e.TransactionCurrency == "" {
		for k, v := range e.Facts {
			if _, ok := ParseDecimal(v); ok && (k == "gross" || k == "net" || k == "tax") {
				return bad("transaction_currency", "required when the event carries monetary facts")
			}
		}
	}
	return nil
}

// ComputeHash returns the integrity hash of everything that makes the event what it is, independent of delivery
// metadata. Map keys are ordered by encoding/json, so the hash is stable.
func (e *AccountingEvent) ComputeHash() string {
	view := struct {
		T, LE, Sys, OT, OID, SE string
		V                       int
		Occ                     time.Time
		Eff, Doc, Post          *time.Time
		Ccy, Cp                 string
		F, D                    map[string]string
		Ev                      []string
		Sup                     string `json:",omitempty"`
	}{e.EventType, e.LegalEntityID, e.SourceSystem, e.SourceObjectType, e.SourceObjectID, e.SourceEventID, e.EventVersion, e.OccurredAt.UTC(),
		e.EffectiveAt, e.DocumentDate, e.PostingDateRequested, e.TransactionCurrency, e.CounterpartyID, e.Facts, e.Dimensions, sortedCopy(e.EvidenceRefs), e.SupersedesEventID}
	b, _ := json.Marshal(view)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// NewEventID returns a UUIDv7 (time-ordered) identifier.
func NewEventID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
