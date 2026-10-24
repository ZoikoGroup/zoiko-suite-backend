package kernel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// OutEvent is a domain event written to the transactional outbox in the same transaction as the change it reports.
type OutEvent struct {
	Fact          string // e.g. "book.created": becomes com.zoikosuite.accounting.kernel.<fact>
	AggregateType string
	AggregateID   string
	Actor         string
	CorrelationID string
	LegalEntityID string
	Data          map[string]any
}

// RuleEvent is append-only evidence of a rule-set lifecycle step.
type RuleEvent struct {
	Kind   string         `json:"kind"`
	Actor  string         `json:"actor"`
	Detail map[string]any `json:"detail,omitempty"`
	At     time.Time      `json:"at"`
}

// Filters.
type BookFilter struct {
	LegalEntityID string
	Status        string
	Limit         int
}
type EventFilter struct {
	LegalEntityID string
	EventType     string
	SourceObject  string
	Limit         int
}
type RuleFilter struct {
	EventType string
	Status    string
	Name      string
	Limit     int
}

// Dimension is a financial dimension of a tenant (department, cost centre, project...).
type Dimension struct {
	DimensionCode string    `json:"dimension_code"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// DimensionValue is one value of a dimension, effective-dated and optionally scoped to one legal entity. Values are
// retired, never renamed or deleted, so a posted line keeps its historical meaning.
type DimensionValue struct {
	ValueID         string     `json:"value_id"`
	DimensionCode   string     `json:"dimension_code"`
	ValueCode       string     `json:"value_code"`
	Name            string     `json:"name"`
	LegalEntityID   string     `json:"legal_entity_id,omitempty"`
	ParentValueCode string     `json:"parent_value_code,omitempty"`
	ValidFrom       time.Time  `json:"valid_from"`
	ValidTo         *time.Time `json:"valid_to,omitempty"`
	Status          string     `json:"status"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
}

// Repo is the persistence boundary of the kernel. Every method is tenant-scoped.
type Repo interface {
	InsertBook(ctx context.Context, b *Book, out OutEvent) error
	GetBook(ctx context.Context, tenant, id string) (*Book, error)
	ListBooks(ctx context.Context, tenant string, f BookFilter) ([]Book, error)
	// UpdateBook locks the book, applies fn, advances the version and enqueues the event in one transaction.
	UpdateBook(ctx context.Context, tenant, id string, fn func(*Book) (OutEvent, error)) (*Book, error)

	// InsertEvent stores a new event. When the same producer event (or idempotency key) is already stored with the same
	// payload hash, it returns that event and existing=true; with a different hash it returns an AK-DATA exception.
	InsertEvent(ctx context.Context, e *AccountingEvent, out OutEvent) (stored *AccountingEvent, existing bool, err error)
	GetEvent(ctx context.Context, tenant, id string) (*AccountingEvent, error)
	ListEvents(ctx context.Context, tenant string, f EventFilter) ([]AccountingEvent, error)

	InsertRuleSet(ctx context.Context, r *RuleSet, ev RuleEvent, out OutEvent) error
	GetRuleSet(ctx context.Context, tenant, id string) (*RuleSet, error)
	ListRuleSets(ctx context.Context, tenant string, f RuleFilter) ([]RuleSet, error)
	UpdateRuleSet(ctx context.Context, tenant, id string, fn func(*RuleSet) (RuleEvent, OutEvent, error)) (*RuleSet, error)
	// ActivateRuleSet makes an approved rule set ACTIVE and supersedes the open-ended active version it replaces, in one
	// transaction under the scope lock. It returns the ids it superseded.
	ActivateRuleSet(ctx context.Context, tenant, id string, expectedRowVersion int64, actor string, out OutEvent) (*RuleSet, []string, error)
	ActiveRuleSets(ctx context.Context, tenant, eventType, bookType, framework string, on time.Time) ([]RuleSet, error)
	RuleSetEvents(ctx context.Context, tenant, id string) ([]RuleEvent, error)

	// FindPosting returns the posting consequence of an event in a book under a basis, or ErrNotFound.
	FindPosting(ctx context.Context, tenant, eventID, bookID, basis string) (*PostResult, error)
	// PostUnit writes the journal, its lines, the posting execution, the event-to-posting record, the ledger entries and the
	// outbox event in one transaction, or nothing. When the consequence already exists (a concurrent or repeated post) it
	// returns that one with Existing=true.
	PostUnit(ctx context.Context, u *PostingUnit) (*PostResult, error)
	// RecordPostingFailure stores the evidence of a posting that did not complete and raises it on the outbox.
	RecordPostingFailure(ctx context.Context, f *PostingFailure) error
	// PostingTrace resolves the lineage of a posting: event, rule, execution, journals, lines, relations and outbox ids.
	PostingTrace(ctx context.Context, tenant string, q TraceQuery) (*Trace, error)

	InsertDimension(ctx context.Context, tenant string, d *Dimension) error
	ListDimensions(ctx context.Context, tenant string) ([]Dimension, error)
	InsertDimensionValue(ctx context.Context, tenant string, v *DimensionValue) error
	RetireDimensionValue(ctx context.Context, tenant, valueID, actor string, on time.Time) (*DimensionValue, error)
	ListDimensionValues(ctx context.Context, tenant, dimension string) ([]DimensionValue, error)
}

// Service is the kernel's command side. It owns the rules; the Repo owns the storage and the invariants the database
// holds by itself.
type Service struct {
	Repo  Repo
	Clock func() time.Time
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func today(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Actor is who acts and for which correlation.
type Actor struct {
	PrincipalID   string
	CorrelationID string
}

// ── books ──────────────────────────────────────────────────────────────────────

// CreateBook registers a book in DRAFT.
func (s *Service) CreateBook(ctx context.Context, tenant string, a Actor, b Book) (*Book, error) {
	if err := b.ValidateNew(); err != nil {
		return nil, err
	}
	b.BookID, b.TenantID, b.Status, b.CreatedBy, b.Version = uuid.NewString(), tenant, BookDraft, a.PrincipalID, 1
	b.EffectiveFrom = today(b.EffectiveFrom)
	b.CreatedAt, b.UpdatedAt = s.now(), s.now()
	err := s.Repo.InsertBook(ctx, &b, OutEvent{Fact: "book.created", AggregateType: "accounting-book", AggregateID: b.BookID, Actor: a.PrincipalID, CorrelationID: a.CorrelationID,
		LegalEntityID: b.LegalEntityID, Data: map[string]any{"book_id": b.BookID, "legal_entity_id": b.LegalEntityID, "book_type": b.BookType, "framework": b.AccountingFrameworkID}})
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// SetBookStatus moves a book along its lifecycle. Activating a book never reopens history: a book's postings are
// whatever periods and rules say, this only says whether the book may carry new ones.
func (s *Service) SetBookStatus(ctx context.Context, tenant string, a Actor, id, to string, expectedVersion int64) (*Book, error) {
	return s.Repo.UpdateBook(ctx, tenant, id, func(b *Book) (OutEvent, error) {
		if expectedVersion > 0 && b.Version != expectedVersion {
			return OutEvent{}, ErrVersion
		}
		from := b.Status
		switch {
		case from == BookDraft && (to == BookActive || to == BookRetired),
			from == BookActive && (to == BookSuspended || to == BookRetired),
			from == BookSuspended && (to == BookActive || to == BookRetired):
		default:
			return OutEvent{}, ErrState
		}
		b.Status = to
		return OutEvent{Fact: "book.status-changed", AggregateType: "accounting-book", AggregateID: b.BookID, Actor: a.PrincipalID, CorrelationID: a.CorrelationID,
			LegalEntityID: b.LegalEntityID, Data: map[string]any{"book_id": b.BookID, "from": from, "to": to}}, nil
	})
}

// ── events ─────────────────────────────────────────────────────────────────────

// RegisterEvent validates and stores an accounting event, or returns the one already stored for the same producer event
// (a replay never creates a second consequence). The tenant and the entity are resolved by the caller, never trusted from
// the payload.
func (s *Service) RegisterEvent(ctx context.Context, tenant string, a Actor, e AccountingEvent) (*AccountingEvent, bool, error) {
	e.TenantID = tenant
	if err := e.Validate(); err != nil {
		return nil, false, err
	}
	e.AccountingEventID, e.PayloadHash, e.RegisteredBy, e.RegisteredAt = NewEventID(), e.ComputeHash(), a.PrincipalID, s.now()
	if e.Facts == nil {
		e.Facts = map[string]string{}
	}
	if e.SupersedesEventID != "" {
		prior, err := s.Repo.GetEvent(ctx, tenant, e.SupersedesEventID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, false, exc(FamData, "SUPERSEDED_EVENT_UNKNOWN", "the event this one supersedes does not exist").with("supersedes_event_id", e.SupersedesEventID)
			}
			return nil, false, err
		}
		if prior.LegalEntityID != e.LegalEntityID {
			return nil, false, exc(FamAuth, "ENTITY_MISMATCH", "a correction belongs to the legal entity of the event it corrects")
		}
	}
	return s.Repo.InsertEvent(ctx, &e, OutEvent{Fact: "event.registered", AggregateType: "accounting-event", AggregateID: e.AccountingEventID, Actor: a.PrincipalID,
		CorrelationID: e.CorrelationID, LegalEntityID: e.LegalEntityID,
		Data: map[string]any{"accounting_event_id": e.AccountingEventID, "event_type": e.EventType, "event_version": e.EventVersion, "legal_entity_id": e.LegalEntityID,
			"source_system": e.SourceSystem, "source_event_id": e.SourceEventID, "payload_hash": e.PayloadHash}})
}

// ── rule sets ──────────────────────────────────────────────────────────────────

// CreateRuleSet validates and stores a draft rule-set version.
func (s *Service) CreateRuleSet(ctx context.Context, tenant string, a Actor, r RuleSet) (*RuleSet, error) {
	r.RuleSetID, r.TenantID, r.Status, r.CreatedBy, r.RowVersion = uuid.NewString(), tenant, RuleDraft, a.PrincipalID, 1
	r.EffectiveFrom = today(r.EffectiveFrom)
	r.ApprovedBy, r.ApprovedAt, r.ActivatedBy, r.ActivatedAt, r.TestsPassedHash = "", nil, "", nil, ""
	r.CreatedAt, r.UpdatedAt = s.now(), s.now()
	if err := r.Validate(); err != nil {
		return nil, err
	}
	r.ContentHash = HashContent(r.Content, r.TestPack)
	err := s.Repo.InsertRuleSet(ctx, &r, RuleEvent{Kind: "CREATED", Actor: a.PrincipalID, At: r.CreatedAt}, ruleOut("rule-set.created", &r, a, nil))
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func ruleOut(fact string, r *RuleSet, a Actor, extra map[string]any) OutEvent {
	d := map[string]any{"posting_rule_set_id": r.RuleSetID, "name": r.Name, "version": r.Version, "event_type": r.EventType, "book_type": r.BookType,
		"framework": r.FrameworkID, "content_hash": r.ContentHash}
	for k, v := range extra {
		d[k] = v
	}
	return OutEvent{Fact: fact, AggregateType: "posting-rule-set", AggregateID: r.RuleSetID, Actor: a.PrincipalID, CorrelationID: a.CorrelationID, Data: d}
}

// TestRuleSet runs the rule set's regression pack through the real engine for the given book context. A pass moves a
// draft to TEST and binds the pass to the exact content hash; a failure leaves it a draft and records why.
func (s *Service) TestRuleSet(ctx context.Context, tenant string, a Actor, id string, bookID string, expectedVersion int64) (*RuleSet, []string, error) {
	rs, err := s.Repo.GetRuleSet(ctx, tenant, id)
	if err != nil {
		return nil, nil, err
	}
	bk, err := s.Repo.GetBook(ctx, tenant, bookID)
	if err != nil {
		return nil, nil, err
	}
	if bk.BookType != rs.BookType || bk.AccountingFrameworkID != rs.FrameworkID {
		return nil, nil, exc(FamRule, "BOOK_NOT_APPLICABLE", "the book is %s / %s and the rule set is for %s / %s", bk.BookType, bk.AccountingFrameworkID, rs.BookType, rs.FrameworkID)
	}
	if rs.Status != RuleDraft && rs.Status != RuleTest {
		return nil, nil, ErrState
	}
	failed := RunTestPack(ctx, rs, *bk)
	var res *RuleSet
	res, err = s.Repo.UpdateRuleSet(ctx, tenant, id, func(r *RuleSet) (RuleEvent, OutEvent, error) {
		if expectedVersion > 0 && r.RowVersion != expectedVersion {
			return RuleEvent{}, OutEvent{}, ErrVersion
		}
		if r.Status != RuleDraft && r.Status != RuleTest {
			return RuleEvent{}, OutEvent{}, ErrState
		}
		if r.ContentHash != rs.ContentHash {
			return RuleEvent{}, OutEvent{}, ErrVersion
		}
		if len(failed) > 0 {
			r.Status, r.TestsPassedHash = RuleDraft, ""
			return RuleEvent{Kind: "TEST_FAILED", Actor: a.PrincipalID, Detail: map[string]any{"failed": failed}}, ruleOut("rule-set.test-failed", r, a, map[string]any{"failed_cases": len(failed)}), nil
		}
		r.Status, r.TestsPassedHash = RuleTest, r.ContentHash
		return RuleEvent{Kind: "TEST_PASSED", Actor: a.PrincipalID, Detail: map[string]any{"cases": len(r.TestPack), "content_hash": r.ContentHash}}, ruleOut("rule-set.tested", r, a, map[string]any{"cases": len(r.TestPack)}), nil
	})
	return res, failed, err
}

// ApproveRuleSet approves a tested rule set. The approver is not the author; the database refuses it otherwise.
func (s *Service) ApproveRuleSet(ctx context.Context, tenant string, a Actor, id string, expectedVersion int64) (*RuleSet, error) {
	return s.Repo.UpdateRuleSet(ctx, tenant, id, func(r *RuleSet) (RuleEvent, OutEvent, error) {
		if expectedVersion > 0 && r.RowVersion != expectedVersion {
			return RuleEvent{}, OutEvent{}, ErrVersion
		}
		if r.Status != RuleTest {
			return RuleEvent{}, OutEvent{}, ErrState
		}
		if a.PrincipalID == r.CreatedBy {
			return RuleEvent{}, OutEvent{}, ErrSelfApproval
		}
		now := s.now()
		r.Status, r.ApprovedBy, r.ApprovedAt = RuleApproved, a.PrincipalID, &now
		return RuleEvent{Kind: "APPROVED", Actor: a.PrincipalID}, ruleOut("rule-set.approved", r, a, nil), nil
	})
}

// ActivateRuleSet makes an approved rule set the one in force from its start date. It supersedes the version it
// replaces; two rule sets never claim the same scope on the same date.
func (s *Service) ActivateRuleSet(ctx context.Context, tenant string, a Actor, id string, expectedVersion int64) (*RuleSet, []string, error) {
	cur, err := s.Repo.GetRuleSet(ctx, tenant, id)
	if err != nil {
		return nil, nil, err
	}
	if cur.Status != RuleApproved {
		return nil, nil, ErrState
	}
	if a.PrincipalID == cur.CreatedBy {
		return nil, nil, ErrSelfApproval
	}
	return s.Repo.ActivateRuleSet(ctx, tenant, id, expectedVersion, a.PrincipalID, ruleOut("rule-set.activated", cur, a, nil))
}

// RetireRuleSet stops a rule set from carrying new postings. History that used it is untouched.
func (s *Service) RetireRuleSet(ctx context.Context, tenant string, a Actor, id, reason string, expectedVersion int64) (*RuleSet, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, exc(FamData, "REASON_REQUIRED", "a reason is required to retire a rule set")
	}
	return s.Repo.UpdateRuleSet(ctx, tenant, id, func(r *RuleSet) (RuleEvent, OutEvent, error) {
		if expectedVersion > 0 && r.RowVersion != expectedVersion {
			return RuleEvent{}, OutEvent{}, ErrVersion
		}
		if r.Status != RuleActive && r.Status != RuleSuperseded {
			return RuleEvent{}, OutEvent{}, ErrState
		}
		if r.Status == RuleActive {
			end := today(s.now())
			if end.After(r.EffectiveFrom) {
				r.EffectiveTo = &end
			}
		}
		r.Status = RuleRetired
		return RuleEvent{Kind: "RETIRED", Actor: a.PrincipalID, Detail: map[string]any{"reason": reason}}, ruleOut("rule-set.retired", r, a, nil), nil
	})
}

// ReturnToDraft withdraws a rule set from test or approval so its content can be corrected.
func (s *Service) ReturnToDraft(ctx context.Context, tenant string, a Actor, id string, expectedVersion int64) (*RuleSet, error) {
	return s.Repo.UpdateRuleSet(ctx, tenant, id, func(r *RuleSet) (RuleEvent, OutEvent, error) {
		if expectedVersion > 0 && r.RowVersion != expectedVersion {
			return RuleEvent{}, OutEvent{}, ErrVersion
		}
		if r.Status != RuleTest && r.Status != RuleApproved {
			return RuleEvent{}, OutEvent{}, ErrState
		}
		r.Status, r.TestsPassedHash, r.ApprovedBy, r.ApprovedAt = RuleDraft, "", "", nil
		return RuleEvent{Kind: "RETURNED_TO_DRAFT", Actor: a.PrincipalID}, ruleOut("rule-set.returned-to-draft", r, a, nil), nil
	})
}

// ── evaluation ─────────────────────────────────────────────────────────────────

// Resolver supplies what evaluation needs besides the stored kernel objects.
type Resolver interface {
	Directory
	// FXBasis returns the rate basis for converting from a currency into the book's functional currency, or nil.
	FXBasis(ctx context.Context, policyID, from, to string, asOf time.Time) (*FXBasis, error)
}

// EvaluateRequest names an event and the book whose consequence is wanted.
type EvaluateRequest struct {
	AccountingEventID string    `json:"accounting_event_id"`
	BookID            string    `json:"book_id"`
	PostingDate       time.Time `json:"posting_date,omitempty"`
	FX                *FXBasis  `json:"fx_basis,omitempty"`
}

// Evaluate is EvaluatePosting: it selects the one active rule set for the event in the book, runs the engine and returns
// the journal proposal with its provenance. Nothing is written. An exception is returned as an error of type
// *Exception for the caller to present.
func (s *Service) Evaluate(ctx context.Context, tenant string, req EvaluateRequest, res Resolver) (*Proposal, error) {
	ev, err := s.Repo.GetEvent(ctx, tenant, req.AccountingEventID)
	if err != nil {
		return nil, err
	}
	bk, err := s.Repo.GetBook(ctx, tenant, req.BookID)
	if err != nil {
		return nil, err
	}
	if bk.LegalEntityID != ev.LegalEntityID {
		return nil, exc(FamAuth, "ENTITY_MISMATCH", "the book belongs to another legal entity than the event")
	}
	pd := req.PostingDate
	if pd.IsZero() {
		switch {
		case ev.PostingDateRequested != nil:
			pd = *ev.PostingDateRequested
		case ev.DocumentDate != nil:
			pd = *ev.DocumentDate
		default:
			pd = ev.OccurredAt
		}
	}
	pd = today(pd)
	sets, err := s.Repo.ActiveRuleSets(ctx, tenant, ev.EventType, bk.BookType, bk.AccountingFrameworkID, pd)
	if err != nil {
		return nil, err
	}
	switch len(sets) {
	case 0:
		return nil, exc(FamRule, "NO_RULE", "no active rule set for %s in a %s book under %s on %s", ev.EventType, bk.BookType, bk.AccountingFrameworkID, pd.Format("2006-01-02")).
			with("event_type", ev.EventType).with("book_type", bk.BookType)
	case 1:
	default:
		ids := make([]string, 0, len(sets))
		for _, r := range sets {
			ids = append(ids, r.Name+" "+r.Version)
		}
		return nil, exc(FamRule, "AMBIGUOUS_RULE", "%d active rule sets claim this event on %s", len(sets), pd.Format("2006-01-02")).with("candidates", ids)
	}
	if req.FX != nil && res != nil {
		// Where a rate source is configured, a caller does not bring its own rate: that is what the override workflow is for.
		if enf, ok := res.(interface{ RejectSuppliedFX() bool }); ok && enf.RejectSuppliedFX() {
			return nil, exc(FamFX, "RATE_OVERRIDE_NOT_ALLOWED", "an exchange rate comes from the rate store; a hand-keyed rate goes through the override workflow, which needs a reason and a second person")
		}
	}
	fx := req.FX
	if fx == nil && bk.FunctionalCurrency != ev.TransactionCurrency && ev.TransactionCurrency != "" && res != nil {
		var ferr error
		if fx, ferr = res.FXBasis(ctx, sets[0].Content.FXPolicyID, ev.TransactionCurrency, bk.FunctionalCurrency, pd); ferr != nil {
			if e, isExc := AsException(ferr); isExc {
				return nil, e
			}
			return nil, exc(FamFX, "RATE_UNAVAILABLE", "no exchange rate could be resolved: %v", ferr)
		}
	}
	p, err := Evaluate(ctx, EvalInput{Event: ev, Book: bk, Rule: &sets[0], PostingDate: pd, FX: fx, Directory: res})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ── dimensions ─────────────────────────────────────────────────────────────────

// CreateDimension registers a dimension.
func (s *Service) CreateDimension(ctx context.Context, tenant string, a Actor, code, name string) (*Dimension, error) {
	if !codeRE.MatchString(code) || strings.TrimSpace(name) == "" || len(name) > 120 {
		return nil, exc(FamDim, "DIMENSION_INVALID", "a dimension code and a name")
	}
	d := &Dimension{DimensionCode: code, Name: name, Status: "ACTIVE", CreatedBy: a.PrincipalID, CreatedAt: s.now()}
	if err := s.Repo.InsertDimension(ctx, tenant, d); err != nil {
		return nil, err
	}
	return d, nil
}

// CreateDimensionValue registers a value, effective from a date and optionally scoped to one legal entity.
func (s *Service) CreateDimensionValue(ctx context.Context, tenant string, a Actor, v DimensionValue) (*DimensionValue, error) {
	if !codeRE.MatchString(v.DimensionCode) || strings.TrimSpace(v.ValueCode) == "" || len(v.ValueCode) > 120 || strings.TrimSpace(v.Name) == "" || len(v.Name) > 200 {
		return nil, exc(FamDim, "DIMENSION_VALUE_INVALID", "a dimension, a value code and a name")
	}
	if v.LegalEntityID != "" {
		if _, err := uuid.Parse(v.LegalEntityID); err != nil {
			return nil, exc(FamDim, "DIMENSION_VALUE_INVALID", "legal_entity_id is not an entity id")
		}
	}
	if v.ValidFrom.IsZero() {
		return nil, exc(FamDim, "DIMENSION_VALUE_INVALID", "valid_from is required")
	}
	v.ValidFrom = today(v.ValidFrom)
	if v.ValidTo != nil && !v.ValidTo.After(v.ValidFrom) {
		return nil, exc(FamDim, "DIMENSION_VALUE_INVALID", "valid_to must be after valid_from")
	}
	v.ValueID, v.Status, v.CreatedBy, v.CreatedAt = uuid.NewString(), "ACTIVE", a.PrincipalID, s.now()
	if err := s.Repo.InsertDimensionValue(ctx, tenant, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// IsConflict reports a store-level conflict that the HTTP layer maps to 409.
func IsConflict(err error) bool {
	return errors.Is(err, ErrExists) || errors.Is(err, ErrVersion) || errors.Is(err, ErrState) || errors.Is(err, ErrOverlap) || errors.Is(err, ErrIdempotency)
}

var _ = fmt.Sprintf
