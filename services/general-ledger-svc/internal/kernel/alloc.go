package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Allocations (standard §21, §35): a source pool of the period is spread over recipients by a driver. The allocated total always
// equals the pool: each recipient's share is rounded to the currency's minor unit and any residual is posted on an explicit rounding
// line, within a bound. The driver snapshot the run used is retained. One live run exists per rule, book and period.

// Allocation rule statuses.
const (
	AllocDraft   = "DRAFT"
	AllocActive  = "ACTIVE"
	AllocRetired = "RETIRED"
)

// AllocFixed is the driver of a rule whose recipients carry fixed percentages.
const AllocFixed = "FIXED"

// MapAllocationRounding is the mapping key of the account that carries a run's rounding residual.
const MapAllocationRounding = "ALLOCATION_ROUNDING"

// AllocRecipient is where part of the pool goes: an account, with the dimensions the allocated cost is to carry.
type AllocRecipient struct {
	Key        string            `json:"key"`
	Account    string            `json:"account_code"`
	Dimensions map[string]string `json:"dimensions,omitempty"`
	Percent    string            `json:"percent,omitempty"` // FIXED rules only
}

// AllocContent is the governed content of an allocation rule version.
type AllocContent struct {
	Name             string            `json:"name"`
	SourceAccounts   []string          `json:"source_accounts"`
	SourceDimensions map[string]string `json:"source_dimensions,omitempty"` // the pool is the entries that carry these dimensions
	Driver           string            `json:"driver"`                      // FIXED, or the name of the driver the run supplies
	Recipients       []AllocRecipient  `json:"recipients"`
}

// AllocRule is one immutable version of an allocation rule.
type AllocRule struct {
	RuleID        string       `json:"allocation_rule_id"`
	RuleCode      string       `json:"rule_code"`
	Version       int          `json:"version"`
	Status        string       `json:"status"`
	Content       AllocContent `json:"content"`
	ContentHash   string       `json:"content_hash"`
	EffectiveFrom string       `json:"effective_from"`
	CreatedBy     string       `json:"created_by"`
	CreatedAt     time.Time    `json:"created_at"`
	ApprovedBy    string       `json:"approved_by,omitempty"`
	ApprovedAt    *time.Time   `json:"approved_at,omitempty"`
	RetiredBy     string       `json:"retired_by,omitempty"`
	RetiredAt     *time.Time   `json:"retired_at,omitempty"`
}

// PoolLine is the net debit balance of one source account in the period.
type PoolLine struct {
	Account string `json:"account_code"`
	Net     string `json:"net_debit"`
}

// AllocShare is one recipient's part of a run.
type AllocShare struct {
	Key        string            `json:"key"`
	Account    string            `json:"account_code"`
	Dimensions map[string]string `json:"dimensions,omitempty"`
	Driver     string            `json:"driver"`
	Exact      string            `json:"exact_share"`
	Amount     string            `json:"amount"`
}

// AllocPopulation is the immutable record of what a run took and distributed.
type AllocPopulation struct {
	Currency       string            `json:"currency"`
	PeriodStart    string            `json:"period_start"`
	AsOf           string            `json:"as_of"`
	RuleCode       string            `json:"rule_code"`
	RuleVersion    int               `json:"rule_version"`
	RuleHash       string            `json:"rule_content_hash"`
	Pool           []PoolLine        `json:"pool"`
	DriverName     string            `json:"driver"`
	DriverRef      string            `json:"driver_ref,omitempty"`
	Drivers        map[string]string `json:"driver_snapshot"`
	Shares         []AllocShare      `json:"shares"`
	Residual       string            `json:"rounding_residual"`
	RoundingAcct   string            `json:"rounding_account,omitempty"`
	SourceDims     map[string]string `json:"source_dimensions,omitempty"`
	Allocated      string            `json:"allocated_total"`
	PostingDateStr string            `json:"posting_date"`
}

// AllocRun is a completed allocation.
type AllocRun struct {
	RunID             string          `json:"allocation_run_id"`
	RuleID            string          `json:"allocation_rule_id"`
	LegalEntityID     string          `json:"legal_entity_id"`
	BookID            string          `json:"accounting_book_id"`
	FiscalPeriod      string          `json:"fiscal_period"`
	Status            string          `json:"status"`
	LedgerWatermark   int64           `json:"ledger_watermark"`
	PoolAmount        string          `json:"pool_amount"`
	Population        AllocPopulation `json:"population"`
	JournalID         string          `json:"journal_id,omitempty"`
	JournalNumber     string          `json:"journal_number,omitempty"`
	ReversalJournalID string          `json:"reversal_journal_id,omitempty"`
	ReversalNumber    string          `json:"reversal_journal_number,omitempty"`
	CreatedBy         string          `json:"created_by"`
	CreatedAt         time.Time       `json:"created_at"`
	ReversedBy        string          `json:"reversed_by,omitempty"`
	ReversedAt        *time.Time      `json:"reversed_at,omitempty"`
	ReversalReason    string          `json:"reversal_reason,omitempty"`
	Existing          bool            `json:"existing,omitempty"`
}

// AllocPlan is what the repository writes in one transaction: the journal and the run.
type AllocPlan struct {
	Tenant      string
	Book        Book
	Rule        AllocRule
	Run         AllocRun
	PostingDate time.Time
	PeriodStart time.Time
	SourceAccts []string
	SourceDims  map[string]string
	Lines       []AllocLine
	Actor       string
	Correlation string
}

// AllocLine is one line of the allocation journal, signed: positive is a debit.
type AllocLine struct {
	Account     string
	Amount      string
	Dimensions  map[string]string
	Description string
	Ref         string
}

// AllocRepo is the persistence boundary of allocations.
type AllocRepo interface {
	InsertAllocRule(ctx context.Context, tenant string, r *AllocRule, out OutEvent) (*AllocRule, error)
	GetAllocRule(ctx context.Context, tenant, id string) (*AllocRule, error)
	ListAllocRules(ctx context.Context, tenant, code string) ([]AllocRule, error)
	// ApproveAllocRule activates a draft and retires the version of the same code it replaces, atomically.
	ApproveAllocRule(ctx context.Context, tenant, id, actor string, at time.Time, out OutEvent) (*AllocRule, error)
	RetireAllocRule(ctx context.Context, tenant, id, actor string, at time.Time, out OutEvent) (*AllocRule, error)

	// PoolBalances reads the net debit of the source accounts in a book between two dates, from one consistent snapshot, with its watermark.
	PoolBalances(ctx context.Context, tenant, entity, bookCode string, accounts []string, dims map[string]string, from, to time.Time) ([]PoolLine, int64, error)
	GetAllocRun(ctx context.Context, tenant, id string) (*AllocRun, error)
	ListAllocRuns(ctx context.Context, tenant, entity, period string) ([]AllocRun, error)
	LiveAllocRun(ctx context.Context, tenant, ruleID, bookID, period string) (*AllocRun, error)
	// WriteAllocation posts the journal and records the run atomically; it returns the live run when one exists and refuses (AK-CONC)
	// when entries joined the pool after the watermark.
	WriteAllocation(ctx context.Context, p *AllocPlan) (*AllocRun, error)
	ReverseAllocation(ctx context.Context, tenant, id, actor, reason string, at, postingDate time.Time, book Book) (*AllocRun, error)
}

// AllocService is the command side of allocations.
type AllocService struct {
	Repo  AllocRepo
	Books Repo
	Dir   Directory
	Gate  PeriodGate
	Clock func() time.Time
}

func (s *AllocService) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func allocBad(f, why string) error {
	return exc(FamData, "ALLOCATION_INVALID", "%s: %s", f, why).with("field", f)
}

// AllocRuleRequest creates a draft allocation rule version.
type AllocRuleRequest struct {
	RuleCode      string       `json:"rule_code"`
	EffectiveFrom string       `json:"effective_from"`
	Content       AllocContent `json:"content"`
}

// Validate checks the content of a rule.
func (c *AllocContent) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return allocBad("content.name", "required")
	}
	if len(c.SourceAccounts) == 0 {
		return allocBad("content.source_accounts", "at least one source account")
	}
	src := map[string]bool{}
	for _, a := range c.SourceAccounts {
		if strings.TrimSpace(a) == "" || src[a] {
			return allocBad("content.source_accounts", "distinct, non-empty account codes")
		}
		src[a] = true
	}
	if len(c.Recipients) < 2 {
		return allocBad("content.recipients", "at least two recipients: a pool spread over one is a reclassification")
	}
	if strings.TrimSpace(c.Driver) == "" {
		return allocBad("content.driver", "FIXED, or the name of a driver such as headcount")
	}
	sum := new(big.Rat)
	keys := map[string]bool{}
	for _, r := range c.Recipients {
		if !codeRE.MatchString(r.Key) || keys[r.Key] {
			return allocBad("content.recipients", "each recipient has a distinct key of letters, digits, dot, dash or underscore")
		}
		keys[r.Key] = true
		if strings.TrimSpace(r.Account) == "" || src[r.Account] {
			return allocBad("content.recipients", fmt.Sprintf("recipient %s needs an account that is not one of the source accounts", r.Key))
		}
		if c.Driver == AllocFixed {
			p, ok := new(big.Rat).SetString(r.Percent)
			if !ok || p.Sign() <= 0 {
				return allocBad("content.recipients", fmt.Sprintf("recipient %s needs a positive percent under a FIXED driver", r.Key))
			}
			sum.Add(sum, p)
		} else if r.Percent != "" {
			return allocBad("content.recipients", "percentages belong to FIXED rules; a named driver takes its values from the run")
		}
	}
	if c.Driver == AllocFixed && sum.Cmp(big.NewRat(100, 1)) != 0 {
		return allocBad("content.recipients", fmt.Sprintf("the percentages add up to %s, not 100", sum.FloatString(4)))
	}
	return nil
}

func (c *AllocContent) hash() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CreateRule creates the next draft version of a rule code.
func (s *AllocService) CreateRule(ctx context.Context, tenant string, a Actor, req AllocRuleRequest) (*AllocRule, error) {
	if !codeRE.MatchString(req.RuleCode) {
		return nil, allocBad("rule_code", "1-63 letters, digits, dot, dash or underscore")
	}
	from, err := time.Parse("2006-01-02", req.EffectiveFrom)
	if err != nil {
		return nil, allocBad("effective_from", "a date, YYYY-MM-DD")
	}
	if err := req.Content.Validate(); err != nil {
		return nil, err
	}
	r := &AllocRule{RuleID: uuid.NewString(), RuleCode: req.RuleCode, Status: AllocDraft, Content: req.Content, ContentHash: req.Content.hash(), EffectiveFrom: from.Format("2006-01-02"),
		CreatedBy: a.PrincipalID, CreatedAt: s.now()}
	return s.Repo.InsertAllocRule(ctx, tenant, r, OutEvent{Fact: "allocation-rule.created", AggregateType: "allocation-rule", AggregateID: r.RuleID, Actor: a.PrincipalID, CorrelationID: a.CorrelationID,
		Data: map[string]any{"allocation_rule_id": r.RuleID, "rule_code": r.RuleCode, "content_hash": r.ContentHash}})
}

// ApproveRule activates a draft; someone other than its author must do it. The version it replaces is retired.
func (s *AllocService) ApproveRule(ctx context.Context, tenant string, a Actor, id string) (*AllocRule, error) {
	r, err := s.Repo.GetAllocRule(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	if r.Status != AllocDraft {
		return nil, ErrState
	}
	if r.CreatedBy == a.PrincipalID {
		return nil, ErrSelfApproval
	}
	return s.Repo.ApproveAllocRule(ctx, tenant, id, a.PrincipalID, s.now(), OutEvent{Fact: "allocation-rule.activated", AggregateType: "allocation-rule", AggregateID: id, Actor: a.PrincipalID,
		CorrelationID: a.CorrelationID, Data: map[string]any{"allocation_rule_id": id, "rule_code": r.RuleCode, "version": r.Version, "content_hash": r.ContentHash}})
}

// RetireRule retires a rule version.
func (s *AllocService) RetireRule(ctx context.Context, tenant string, a Actor, id string) (*AllocRule, error) {
	return s.Repo.RetireAllocRule(ctx, tenant, id, a.PrincipalID, s.now(), OutEvent{Fact: "allocation-rule.retired", AggregateType: "allocation-rule", AggregateID: id, Actor: a.PrincipalID,
		CorrelationID: a.CorrelationID, Data: map[string]any{"allocation_rule_id": id}})
}

// AllocRunRequest asks for an allocation of one rule in one book and period.
type AllocRunRequest struct {
	RuleID       string            `json:"allocation_rule_id"`
	BookID       string            `json:"book_id"`
	FiscalPeriod string            `json:"fiscal_period"`
	AsOf         string            `json:"as_of,omitempty"`
	Drivers      map[string]string `json:"drivers,omitempty"`
	DriverRef    string            `json:"driver_ref,omitempty"`
}

// Distribute spreads a pool over weights: each share is rounded half-even to the scale, and the residual that rounding leaves is
// returned separately so it can be posted on an explicit line. It returns the exact shares too, for the record.
func Distribute(pool *big.Rat, weights []*big.Rat, scale int) (amounts, exact []*big.Rat, residual *big.Rat) {
	total := new(big.Rat)
	for _, w := range weights {
		total.Add(total, w)
	}
	residual = new(big.Rat).Set(pool)
	for _, w := range weights {
		e := new(big.Rat).Mul(pool, w)
		e.Quo(e, total)
		exact = append(exact, e)
		r := new(big.Rat).SetFrac(RoundMinor(e, scale, "HALF_EVEN"), pow10(scale))
		amounts = append(amounts, r)
		residual.Sub(residual, r)
	}
	return
}

// Run allocates a pool. A live run for the rule, book and period is returned unchanged.
func (s *AllocService) Run(ctx context.Context, tenant string, a Actor, req AllocRunRequest) (*AllocRun, error) {
	if _, err := uuid.Parse(req.RuleID); err != nil {
		return nil, allocBad("allocation_rule_id", "an allocation rule id")
	}
	if _, err := uuid.Parse(req.BookID); err != nil {
		return nil, allocBad("book_id", "an accounting book id")
	}
	pstart, pend, ok := periodBounds(req.FiscalPeriod)
	if !ok {
		return nil, allocBad("fiscal_period", "a period, YYYY-MM")
	}
	asOf := pend
	if req.AsOf != "" {
		var err error
		if asOf, err = time.Parse("2006-01-02", req.AsOf); err != nil || asOf.Before(pstart) || asOf.After(pend) {
			return nil, allocBad("as_of", "a date inside the fiscal period")
		}
	}
	if asOf.After(today(s.now())) {
		return nil, allocBad("as_of", "not in the future")
	}
	rule, err := s.Repo.GetAllocRule(ctx, tenant, req.RuleID)
	if err != nil {
		return nil, err
	}
	if rule.Status != AllocActive {
		return nil, exc(FamRule, "ALLOCATION_RULE_NOT_ACTIVE", "allocation rule %s version %d is %s", rule.RuleCode, rule.Version, strings.ToLower(rule.Status))
	}
	if from, _ := time.Parse("2006-01-02", rule.EffectiveFrom); asOf.Before(from) {
		return nil, exc(FamRule, "ALLOCATION_RULE_NOT_EFFECTIVE", "the rule takes effect on %s", rule.EffectiveFrom)
	}
	bk, err := s.Books.GetBook(ctx, tenant, req.BookID)
	if err != nil {
		return nil, err
	}
	if !bk.InForce(asOf) {
		return nil, exc(FamRule, "BOOK_NOT_IN_FORCE", "book %s is %s on %s", bk.BookCode, strings.ToLower(bk.Status), asOf.Format("2006-01-02"))
	}
	if live, err := s.Repo.LiveAllocRun(ctx, tenant, rule.RuleID, bk.BookID, req.FiscalPeriod); err == nil {
		live.Existing = true
		return live, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	c := rule.Content
	pool, wm, err := s.Repo.PoolBalances(ctx, tenant, bk.LegalEntityID, bk.BookCode, c.SourceAccounts, c.SourceDimensions, pstart, asOf)
	if err != nil {
		return nil, err
	}
	poolTotal := new(big.Rat)
	for _, l := range pool {
		poolTotal.Add(poolTotal, dec(l.Net))
	}
	if poolTotal.Sign() <= 0 {
		return nil, exc(FamData, "ALLOCATION_NO_POOL", "the source accounts carry %s in %s: there is no cost to allocate", poolTotal.FloatString(2), req.FiscalPeriod)
	}
	scale, err := s.Dir.MinorUnits(ctx, bk.FunctionalCurrency, asOf)
	if err != nil {
		return nil, exc(FamExt, "CURRENCY_UNAVAILABLE", "the precision of %s could not be read: %v", bk.FunctionalCurrency, err)
	}
	if scale > 2 {
		return nil, exc(FamData, "LEDGER_PRECISION", "the ledger stores two decimals and %s has %d", bk.FunctionalCurrency, scale)
	}

	// the driver snapshot: fixed percentages, or values the owning system supplies with a reference
	weights := make([]*big.Rat, len(c.Recipients))
	snapshot := map[string]string{}
	if c.Driver == AllocFixed {
		for i, r := range c.Recipients {
			weights[i] = dec(r.Percent)
			snapshot[r.Key] = r.Percent
		}
	} else {
		if strings.TrimSpace(req.DriverRef) == "" {
			return nil, allocBad("driver_ref", fmt.Sprintf("the %s driver values come with a reference to where they were taken from", c.Driver))
		}
		total := new(big.Rat)
		for i, r := range c.Recipients {
			v, found := req.Drivers[r.Key]
			w, okv := new(big.Rat).SetString(v)
			if !found || !okv || w.Sign() < 0 {
				return nil, allocBad("drivers", fmt.Sprintf("a driver value, zero or more, for recipient %s", r.Key))
			}
			weights[i] = w
			total.Add(total, w)
			snapshot[r.Key] = v
		}
		for k := range req.Drivers {
			if _, known := snapshot[k]; !known {
				return nil, allocBad("drivers", fmt.Sprintf("%s is not a recipient of the rule", k))
			}
		}
		if total.Sign() == 0 {
			return nil, allocBad("drivers", "the driver values add up to zero: there is nothing to spread by")
		}
	}

	amounts, exact, residual := Distribute(poolTotal, weights, scale)
	bound := new(big.Rat).SetFrac(big.NewInt(int64(len(c.Recipients))), pow10(scale))
	if new(big.Rat).Abs(residual).Cmp(bound) > 0 {
		return nil, exc(FamBal, "ALLOCATION_RESIDUAL_TOO_LARGE", "rounding left %s, more than one minor unit per recipient", residual.FloatString(scale))
	}

	// every account and dimension the journal will use must be valid for this book on the date
	check := func(code string, dims map[string]string) error {
		info, aerr := s.Dir.ResolveAccount(ctx, AccountRef{AccountCode: code}, bk.BookID, asOf)
		if aerr != nil || info == nil || !info.Active || !info.Postable {
			return exc(FamCOA, "ACCOUNT_UNAVAILABLE", "account %s is not an active, postable account", code).with("account_code", code)
		}
		if len(info.BookIDs) > 0 {
			in := false
			for _, id := range info.BookIDs {
				in = in || id == bk.BookID
			}
			if !in {
				return exc(FamCOA, "ACCOUNT_NOT_IN_BOOK", "account %s is not valid in book %s", code, bk.BookCode).with("account_code", code)
			}
		}
		for d, v := range dims {
			okd, derr := s.Dir.DimensionAllowed(ctx, d, v, bk.LegalEntityID, bk.BookID, asOf)
			if derr != nil || !okd {
				return exc(FamDim, "DIMENSION_INVALID", "%s=%s is not an allowed value of dimension %s for this entity on %s", d, v, d, asOf.Format("2006-01-02")).with("dimension", d)
			}
		}
		return nil
	}
	for _, r := range c.Recipients {
		if err := check(r.Account, r.Dimensions); err != nil {
			return nil, err
		}
	}

	pop := AllocPopulation{Currency: bk.FunctionalCurrency, PeriodStart: pstart.Format("2006-01-02"), AsOf: asOf.Format("2006-01-02"), RuleCode: rule.RuleCode, RuleVersion: rule.Version,
		RuleHash: rule.ContentHash, Pool: pool, DriverName: c.Driver, DriverRef: req.DriverRef, Drivers: snapshot, SourceDims: c.SourceDimensions, PostingDateStr: asOf.Format("2006-01-02")}
	plan := &AllocPlan{Tenant: tenant, Book: *bk, Rule: *rule, PostingDate: asOf, PeriodStart: pstart, SourceAccts: c.SourceAccounts, SourceDims: c.SourceDimensions, Actor: a.PrincipalID, Correlation: a.CorrelationID}
	allocated := new(big.Rat)
	for i, r := range c.Recipients {
		allocated.Add(allocated, amounts[i])
		pop.Shares = append(pop.Shares, AllocShare{Key: r.Key, Account: r.Account, Dimensions: r.Dimensions, Driver: snapshot[r.Key], Exact: exact[i].FloatString(8), Amount: amounts[i].FloatString(scale)})
		if amounts[i].Sign() != 0 {
			plan.Lines = append(plan.Lines, AllocLine{Account: r.Account, Amount: amounts[i].FloatString(scale), Dimensions: r.Dimensions, Description: "allocation " + rule.RuleCode + " to " + r.Key, Ref: "ALLOC_" + r.Key})
		}
	}
	pop.Residual = residual.FloatString(scale)
	if residual.Sign() != 0 {
		info, aerr := s.Dir.ResolveAccount(ctx, AccountRef{MappingKey: MapAllocationRounding}, bk.BookID, asOf)
		if aerr != nil || info == nil || !info.Active || !info.Postable {
			return nil, exc(FamCOA, "ALLOCATION_ACCOUNT_UNAVAILABLE", "the mapping %s does not resolve to a postable account on %s: the rounding residual of %s needs one", MapAllocationRounding, asOf.Format("2006-01-02"), residual.FloatString(scale))
		}
		pop.RoundingAcct = info.Code
		plan.Lines = append(plan.Lines, AllocLine{Account: info.Code, Amount: residual.FloatString(scale), Description: "allocation rounding", Ref: "ALLOC_ROUNDING"})
		allocated.Add(allocated, residual)
	}
	pop.Allocated = allocated.FloatString(scale)
	for _, l := range pool {
		n := dec(l.Net)
		if n.Sign() != 0 {
			plan.Lines = append(plan.Lines, AllocLine{Account: l.Account, Amount: new(big.Rat).Neg(n).FloatString(scale), Dimensions: c.SourceDimensions, Description: "allocation " + rule.RuleCode + " relieves the pool", Ref: "ALLOC_POOL"})
		}
	}
	if s.Gate != nil {
		if err := s.Gate.CheckPostable(ctx, tenant, bk.LegalEntityID, req.FiscalPeriod, asOf, false, "", a.PrincipalID); err != nil {
			return nil, err
		}
	}
	plan.Run = AllocRun{RunID: uuid.NewString(), RuleID: rule.RuleID, LegalEntityID: bk.LegalEntityID, BookID: bk.BookID, FiscalPeriod: req.FiscalPeriod, Status: "POSTED", LedgerWatermark: wm,
		PoolAmount: poolTotal.FloatString(scale), Population: pop, CreatedBy: a.PrincipalID, CreatedAt: s.now()}
	return s.Repo.WriteAllocation(ctx, plan)
}

// Reverse withdraws a posted allocation: its journal is reversed and the period can be allocated again.
func (s *AllocService) Reverse(ctx context.Context, tenant string, a Actor, id, reason string) (*AllocRun, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, allocBad("reason", "required")
	}
	run, err := s.Repo.GetAllocRun(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	if run.Status != "POSTED" {
		return nil, ErrState
	}
	bk, err := s.Books.GetBook(ctx, tenant, run.BookID)
	if err != nil {
		return nil, err
	}
	pd, _ := time.Parse("2006-01-02", run.Population.PostingDateStr)
	if s.Gate != nil {
		if err := s.Gate.CheckPostable(ctx, tenant, run.LegalEntityID, run.FiscalPeriod, pd, false, "", a.PrincipalID); err != nil {
			return nil, err
		}
	}
	return s.Repo.ReverseAllocation(ctx, tenant, id, a.PrincipalID, strings.TrimSpace(reason), s.now(), pd, *bk)
}
