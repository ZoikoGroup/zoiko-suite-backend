package kernel

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DefaultPostingBasis is the basis of the ordinary consequence of an event in a book. A correction is a new accounting
// event, never a second basis of the same event (standard §29).
const DefaultPostingBasis = "PRIMARY"

// PostRequest is the PostEvent command: the accounting event, the book it is posted in and, optionally, the date it takes
// effect. FX carries the rate for a foreign-currency event when no rate source is configured.
type PostRequest struct {
	AccountingEventID string    `json:"accounting_event_id"`
	BookID            string    `json:"book_id"`
	PostingDate       time.Time `json:"posting_date,omitempty"`
	PostingBasis      string    `json:"posting_basis,omitempty"`
	Description       string    `json:"description,omitempty"`
	FX                *FXBasis  `json:"fx_basis,omitempty"`

	OverrideSoftClose       bool   `json:"override_soft_close,omitempty"`
	SoftCloseOverrideReason string `json:"soft_close_override_reason,omitempty"`
}

// PostResult is the posting consequence of an event in a book: the journal, the execution that produced it and the ledger
// watermark it reached. Existing is true when the consequence already existed and nothing was written.
type PostResult struct {
	Existing          bool      `json:"existing"`
	AccountingEventID string    `json:"accounting_event_id"`
	BookID            string    `json:"book_id"`
	PostingBasis      string    `json:"posting_basis"`
	ExecutionID       string    `json:"posting_execution_id"`
	JournalID         string    `json:"journal_id"`
	JournalNumber     string    `json:"journal_number,omitempty"`
	RuleSetID         string    `json:"posting_rule_set_id"`
	RuleVersion       string    `json:"rule_version"`
	ProposalHash      string    `json:"proposal_hash"`
	LedgerWatermark   int64     `json:"ledger_watermark"`
	PostedAt          time.Time `json:"posted_at"`
}

// PostingUnit is everything the repository writes in the one atomic posting transaction (standard §7.1).
type PostingUnit struct {
	Tenant      string
	Actor       Actor
	Event       *AccountingEvent
	Book        *Book
	Proposal    *Proposal
	Basis       string
	ExecutionID string
	JournalID   string
	// FiscalPeriod is resolved from the posting date, never taken from the caller.
	FiscalPeriod    string
	Description     string
	SoftCloseReason string
	PostedAt        time.Time
}

// PostingFailure is the evidence of a posting that could not complete: no journal exists, but the failure is recorded and
// raised so it is never silent (and so the period-close gate can see it).
type PostingFailure struct {
	Tenant      string
	Actor       Actor
	Event       *AccountingEvent
	BookID      string
	Basis       string
	ExecutionID string
	Status      string // FAILED, or QUARANTINED when a person must decide
	Family      string
	Code        string
	Detail      string
	Facts       map[string]any
	At          time.Time
}

// TraceQuery selects the posting trace: by journal, by accounting event or by posting execution.
type TraceQuery struct {
	JournalID         string
	AccountingEventID string
	ExecutionID       string
}

// PeriodGate is the server-side period check of §10. It returns an *Exception of family AK-PERIOD (or AK-EXT when the
// period service cannot answer) and nil when the date may be posted to.
type PeriodGate interface {
	CheckPostable(ctx context.Context, tenant, legalEntityID, fiscalPeriod string, postingDate time.Time, overrideSoftClose bool, overrideReason string, principal string) error
}

// FiscalPeriodOf resolves the period a posting date falls in. The platform names periods YYYY-MM.
func FiscalPeriodOf(d time.Time) string { return d.UTC().Format("2006-01") }

// PostEvent is the kernel's posting command. It is idempotent per accounting event + book + posting basis: when that
// consequence exists it is returned and nothing is evaluated or written, so a later rule version never changes the
// outcome of an event that already posted. Otherwise the event is evaluated, the period gate is applied and the journal is
// written in one transaction. A refusal creates an exception record, never a partial financial consequence.
func (s *Service) PostEvent(ctx context.Context, tenant string, a Actor, req PostRequest, res Resolver, gate PeriodGate) (*PostResult, error) {
	if _, err := uuid.Parse(req.AccountingEventID); err != nil {
		return nil, exc(FamData, "REQUEST_INVALID", "accounting_event_id must be an accounting event id").with("field", "accounting_event_id")
	}
	if _, err := uuid.Parse(req.BookID); err != nil {
		return nil, exc(FamData, "REQUEST_INVALID", "book_id must be an accounting book id").with("field", "book_id")
	}
	basis := strings.ToUpper(strings.TrimSpace(req.PostingBasis))
	if basis == "" {
		basis = DefaultPostingBasis
	}
	if !codeRE.MatchString(strings.ToLower(basis)) || len(basis) > 40 {
		return nil, exc(FamData, "REQUEST_INVALID", "posting_basis is an upper-case code").with("field", "posting_basis")
	}
	if req.OverrideSoftClose && strings.TrimSpace(req.SoftCloseOverrideReason) == "" {
		return nil, exc(FamPeriod, "OVERRIDE_REASON_REQUIRED", "a soft-close override needs a reason")
	}

	if existing, err := s.Repo.FindPosting(ctx, tenant, req.AccountingEventID, req.BookID, basis); err == nil {
		existing.Existing = true
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	ev, err := s.Repo.GetEvent(ctx, tenant, req.AccountingEventID)
	if err != nil {
		return nil, err
	}
	bk, err := s.Repo.GetBook(ctx, tenant, req.BookID)
	if err != nil {
		return nil, err
	}
	execID := uuid.NewString()
	fail := func(err error) (*PostResult, error) {
		s.recordFailure(ctx, tenant, a, ev, req.BookID, basis, execID, err)
		return nil, err
	}

	p, err := s.Evaluate(ctx, tenant, EvaluateRequest{AccountingEventID: req.AccountingEventID, BookID: req.BookID, PostingDate: req.PostingDate, FX: req.FX}, res)
	if err != nil {
		return fail(err)
	}
	if err := checkBalanced(p); err != nil {
		return fail(err)
	}
	if err := checkLedgerPrecision(p); err != nil {
		return fail(err)
	}
	pd, err := time.Parse("2006-01-02", p.PostingDate)
	if err != nil {
		return fail(exc(FamData, "PROPOSAL_INVALID", "the proposal carries no posting date"))
	}
	period := FiscalPeriodOf(pd)
	if gate != nil {
		// As late as possible: the gate is the last thing checked before the transaction, so the window in which a period
		// can close underneath a prepared posting is as small as the period service allows.
		if err := gate.CheckPostable(ctx, tenant, ev.LegalEntityID, period, pd, req.OverrideSoftClose, req.SoftCloseOverrideReason, a.PrincipalID); err != nil {
			return fail(err)
		}
	}

	desc := strings.TrimSpace(req.Description)
	if desc == "" {
		desc = fmt.Sprintf("%s %s/%s", ev.EventType, ev.SourceObjectType, ev.SourceObjectID)
	}
	unit := &PostingUnit{
		Tenant: tenant, Actor: a, Event: ev, Book: bk, Proposal: p, Basis: basis, ExecutionID: execID, JournalID: uuid.NewString(),
		FiscalPeriod: period, Description: desc, SoftCloseReason: strings.TrimSpace(req.SoftCloseOverrideReason), PostedAt: s.now(),
	}
	out, err := s.Repo.PostUnit(ctx, unit)
	if err != nil {
		if _, isExc := AsException(err); isExc {
			return fail(err)
		}
		return nil, err
	}
	return out, nil
}

func (s *Service) recordFailure(ctx context.Context, tenant string, a Actor, ev *AccountingEvent, bookID, basis, execID string, cause error) {
	e, ok := AsException(cause)
	if !ok {
		return // a dependency or database failure is retried; it is not an accounting exception
	}
	status := "FAILED"
	switch e.Family {
	case FamBal, FamData:
		status = "QUARANTINED"
	case FamRule:
		if e.Code == "AMBIGUOUS_RULE" {
			status = "QUARANTINED"
		}
	}
	// Recording is best effort: the refusal is what the caller sees, and a failure to record it must not mask it.
	_ = s.Repo.RecordPostingFailure(ctx, &PostingFailure{
		Tenant: tenant, Actor: a, Event: ev, BookID: bookID, Basis: basis, ExecutionID: execID, Status: status,
		Family: e.Family, Code: e.Code, Detail: e.Detail, Facts: e.Facts, At: s.now(),
	})
}

// checkBalanced is the balance equation of §8.3 on the proposal itself: the functional debits and credits must be equal
// exactly. The engine guarantees it; the posting path does not rely on that.
func checkBalanced(p *Proposal) error {
	dr, ok1 := new(big.Rat).SetString(p.TotalDebitFunc)
	cr, ok2 := new(big.Rat).SetString(p.TotalCreditFunc)
	if !ok1 || !ok2 || dr.Cmp(cr) != 0 || dr.Sign() <= 0 || len(p.Lines) < 2 {
		return exc(FamBal, "UNBALANCED", "debits %s and credits %s are not equal", p.TotalDebitFunc, p.TotalCreditFunc).
			with("debit", p.TotalDebitFunc).with("credit", p.TotalCreditFunc)
	}
	return nil
}

// checkLedgerPrecision refuses a functional amount the ledger cannot hold exactly. The ledger stores two decimals; an
// amount with more would be rounded silently by the database, so it is refused instead.
func checkLedgerPrecision(p *Proposal) error {
	for _, l := range p.Lines {
		if i := strings.IndexByte(l.AmountFunctional, '.'); i >= 0 && len(strings.TrimRight(l.AmountFunctional[i+1:], "0")) > 2 {
			return exc(FamData, "LEDGER_PRECISION", "line %d amount %s needs more than the two decimals the ledger stores", l.LineNo, l.AmountFunctional).
				with("line_no", l.LineNo).with("amount", l.AmountFunctional)
		}
	}
	return nil
}
