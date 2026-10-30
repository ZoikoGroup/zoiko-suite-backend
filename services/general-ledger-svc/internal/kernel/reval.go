package kernel

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Revaluation (standard §24): at period end the open monetary items held in a foreign currency are valued at the closing rate. Open
// items are identified deterministically from the ledger (monetary accounts, foreign transaction currency, a non-zero position),
// the rate comes from the rate store under the book's closing-rate policy, and the unrealized difference is posted as its own
// journal that reverses on the first day of the next period. One run exists per entity, book and period; it keeps the population
// it valued, so it can be reproduced.

// Revaluation accounts are mapping keys of the tenant's chart.
const (
	MapFXUnrealizedGain = "FX_UNREALIZED_GAIN"
	MapFXUnrealizedLoss = "FX_UNREALIZED_LOSS"
)

// DefaultRevalPolicy is the closing-rate policy used when a run names none.
const DefaultRevalPolicy = "CLOSING_ON_DATE"

// ForeignPosition is the position of one counterparty on one monetary account in one foreign currency: what is owed or held in that
// currency, and what the ledger carries it at in the book's currency (revaluations included).
type ForeignPosition struct {
	AccountCode        string `json:"account_code"`
	CounterpartyID     string `json:"counterparty_id,omitempty"`
	Currency           string `json:"currency"`
	TransactionBalance string `json:"transaction_balance"` // signed: debit positive
	CarryingFunctional string `json:"carrying_functional"` // signed: debit positive
}

// RevalItem is one position as the run valued it.
type RevalItem struct {
	ForeignPosition
	Rate               string `json:"rate"`
	RateID             string `json:"rate_id"`
	RevaluedFunctional string `json:"revalued_functional"`
	Difference         string `json:"difference"` // revalued - carrying, signed: positive is a debit to the account
	Outcome            string `json:"outcome"`    // GAIN, LOSS or UNCHANGED
}

// RevalRun is a completed revaluation.
type RevalRun struct {
	RunID             string          `json:"revaluation_run_id"`
	LegalEntityID     string          `json:"legal_entity_id"`
	BookID            string          `json:"accounting_book_id"`
	FiscalPeriod      string          `json:"fiscal_period"`
	RunType           string          `json:"run_type"`
	ValuationDate     string          `json:"valuation_date"`
	PolicyID          string          `json:"policy_id"`
	LedgerWatermark   int64           `json:"ledger_watermark"`
	Population        RevalPopulation `json:"population"`
	ItemCount         int             `json:"item_count"`
	TotalGain         string          `json:"total_gain"`
	TotalLoss         string          `json:"total_loss"`
	JournalID         string          `json:"journal_id,omitempty"`
	JournalNumber     string          `json:"journal_number,omitempty"`
	ReversalJournalID string          `json:"reversal_journal_id,omitempty"`
	ReversalNumber    string          `json:"reversal_journal_number,omitempty"`
	CreatedBy         string          `json:"created_by"`
	CreatedAt         time.Time       `json:"created_at"`
	Existing          bool            `json:"existing,omitempty"`
}

// RevalPopulation is the immutable record of what a run looked at.
type RevalPopulation struct {
	FunctionalCurrency string            `json:"functional_currency"`
	Rates              []RevalRate       `json:"rates"`
	Items              []RevalItem       `json:"items"`
	Exceptions         []ForeignPosition `json:"exceptions,omitempty"`
}

// RevalRate is a rate a run used.
type RevalRate struct {
	Currency string `json:"currency"`
	Rate     string `json:"rate"`
	RateID   string `json:"rate_id"`
	RateType string `json:"rate_type"`
	AsOf     string `json:"as_of"`
	Source   string `json:"source"`
}

// RevalPlan is what the repository writes in one transaction: the run, the revaluation journal and, unless declined, its reversal.
type RevalPlan struct {
	Tenant, Entity, BookID, BookCode, Period, Policy string
	ValuationDate, ReversalDate                      time.Time
	ReversalPeriod                                   string
	Currency                                         string
	Actor                                            string
	At                                               time.Time
	Watermark                                        int64
	Population                                       RevalPopulation
	GainAccount, LossAccount                         string
	TotalGain, TotalLoss                             string
	AutoReverse                                      bool
}

// RevalRepo is the persistence boundary of revaluation.
type RevalRepo interface {
	// ForeignPositions reads the open foreign monetary positions of a book as of a date, from one consistent snapshot, and the
	// ledger watermark of that snapshot.
	ForeignPositions(ctx context.Context, tenant, entity, bookCode, functional string, asOf time.Time) ([]ForeignPosition, int64, error)
	GetRevalRun(ctx context.Context, tenant, entity, bookID, period string) (*RevalRun, error)
	ListRevalRuns(ctx context.Context, tenant, entity string) ([]RevalRun, error)
	// WriteRevaluation records the run and posts its journals atomically. It refuses (AK-CONC) when entries that belong in the
	// population were posted after the watermark, and returns the existing run when one already exists for the scope.
	WriteRevaluation(ctx context.Context, p *RevalPlan) (*RevalRun, error)
}

// RevalService is the command side of revaluation.
type RevalService struct {
	Repo  RevalRepo
	Books Repo
	Rates FXSource
	Dir   Directory
	Gate  PeriodGate
	Clock func() time.Time
}

func (s *RevalService) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

// RevalRequest asks for the period-end revaluation of one book.
type RevalRequest struct {
	BookID        string `json:"book_id"`
	FiscalPeriod  string `json:"fiscal_period"`
	ValuationDate string `json:"valuation_date"`
	PolicyID      string `json:"policy_id,omitempty"`
	AutoReverse   *bool  `json:"auto_reverse,omitempty"`
}

func dec(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return new(big.Rat)
	}
	return r
}

// Run values the open foreign monetary items of a book at the policy's rate and posts the unrealized difference. A run that exists
// for the entity, book and period is returned unchanged (Existing).
func (s *RevalService) Run(ctx context.Context, tenant string, a Actor, req RevalRequest) (*RevalRun, error) {
	bad := func(f, why string) error {
		return exc(FamData, "REVALUATION_INVALID", "%s: %s", f, why).with("field", f)
	}
	if _, err := uuid.Parse(req.BookID); err != nil {
		return nil, bad("book_id", "an accounting book id")
	}
	vd, err := time.Parse("2006-01-02", req.ValuationDate)
	if err != nil {
		return nil, bad("valuation_date", "a date, YYYY-MM-DD")
	}
	if vd.After(today(s.now())) {
		return nil, bad("valuation_date", "not in the future")
	}
	period := strings.TrimSpace(req.FiscalPeriod)
	if period == "" {
		period = FiscalPeriodOf(vd)
	}
	policy := req.PolicyID
	if policy == "" {
		policy = DefaultRevalPolicy
	}
	pol, ok := FXPolicyByID(policy)
	if !ok {
		return nil, bad("policy_id", "an exchange-rate policy")
	}
	bk, err := s.Books.GetBook(ctx, tenant, req.BookID)
	if err != nil {
		return nil, err
	}
	if !bk.InForce(vd) {
		return nil, exc(FamRule, "BOOK_NOT_IN_FORCE", "book %s is %s on %s", bk.BookCode, strings.ToLower(bk.Status), vd.Format("2006-01-02"))
	}
	if existing, err := s.Repo.GetRevalRun(ctx, tenant, bk.LegalEntityID, bk.BookID, period); err == nil {
		existing.Existing = true
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	positions, watermark, err := s.Repo.ForeignPositions(ctx, tenant, bk.LegalEntityID, bk.BookCode, bk.FunctionalCurrency, vd)
	if err != nil {
		return nil, err
	}
	// one rate per currency, from the rate store under the policy; every missing or stale one is reported together
	rates := map[string]*FXBasis{}
	var ccys []string
	for _, p := range positions {
		if _, seen := rates[p.Currency]; !seen {
			rates[p.Currency] = nil
			ccys = append(ccys, p.Currency)
		}
	}
	sort.Strings(ccys)
	var missing []map[string]any
	for _, c := range ccys {
		b, rerr := s.Rates.Basis(ctx, policy, c, bk.FunctionalCurrency, vd)
		if rerr != nil {
			if e, isExc := AsException(rerr); isExc {
				if e.Family == FamExt {
					return nil, e
				}
				missing = append(missing, map[string]any{"currency": c, "code": e.Code, "detail": e.Detail})
				continue
			}
			return nil, rerr
		}
		rates[c] = b
	}
	if len(missing) > 0 {
		return nil, exc(FamFX, "REVALUATION_RATE_UNAVAILABLE", "%d currency(ies) cannot be valued at the %s rate of %s: a revaluation never uses a stale or alternate rate", len(missing), pol.RateType, vd.Format("2006-01-02")).
			with("currencies", missing).with("policy_id", policy)
	}
	scale, err := s.Dir.MinorUnits(ctx, bk.FunctionalCurrency, vd)
	if err != nil {
		return nil, exc(FamExt, "CURRENCY_UNAVAILABLE", "the precision of %s could not be read: %v", bk.FunctionalCurrency, err)
	}

	if scale > 2 {
		return nil, exc(FamData, "LEDGER_PRECISION", "the ledger stores two decimals and %s has %d", bk.FunctionalCurrency, scale)
	}
	pop := RevalPopulation{FunctionalCurrency: bk.FunctionalCurrency, Items: []RevalItem{}}
	for _, c := range ccys {
		b := rates[c]
		pop.Rates = append(pop.Rates, RevalRate{Currency: c, Rate: b.Rate, RateID: b.RateID, RateType: b.RateType, AsOf: b.AsOf, Source: b.Source})
	}
	gain, loss := new(big.Rat), new(big.Rat)
	plan := &RevalPlan{Tenant: tenant, Entity: bk.LegalEntityID, BookID: bk.BookID, BookCode: bk.BookCode, Period: period, Policy: policy, ValuationDate: vd, Currency: bk.FunctionalCurrency,
		Actor: a.PrincipalID, At: s.now(), Watermark: watermark, AutoReverse: req.AutoReverse == nil || *req.AutoReverse}
	for _, p := range positions {
		tx, carrying := dec(p.TransactionBalance), dec(p.CarryingFunctional)
		if tx.Sign() == 0 {
			if carrying.Sign() != 0 {
				pop.Exceptions = append(pop.Exceptions, p) // settled in the foreign currency but still carrying a functional amount: for a person, not for a rate
			}
			continue
		}
		rate := dec(rates[p.Currency].Rate)
		revalued := new(big.Rat).SetFrac(RoundMinor(new(big.Rat).Mul(tx, rate), scale, "HALF_EVEN"), pow10(scale))
		diff := new(big.Rat).Sub(revalued, carrying)
		it := RevalItem{ForeignPosition: p, Rate: rates[p.Currency].Rate, RateID: rates[p.Currency].RateID, RevaluedFunctional: revalued.FloatString(scale), Difference: diff.FloatString(scale), Outcome: "UNCHANGED"}
		switch {
		case diff.Sign() > 0:
			it.Outcome = "GAIN"
			gain.Add(gain, diff)
		case diff.Sign() < 0:
			it.Outcome = "LOSS"
			loss.Sub(loss, diff)
		}
		pop.Items = append(pop.Items, it)
	}
	plan.Population = pop
	plan.TotalGain, plan.TotalLoss = gain.FloatString(scale), loss.FloatString(scale)
	if gain.Sign() == 0 && loss.Sign() == 0 {
		plan.Population = pop // nothing to post: the run records that the items were valued and needed no adjustment
	}

	posting := false
	for _, it := range pop.Items {
		posting = posting || it.Outcome != "UNCHANGED"
	}
	if posting {
		for _, key := range []string{MapFXUnrealizedGain, MapFXUnrealizedLoss} {
			info, aerr := s.Dir.ResolveAccount(ctx, AccountRef{MappingKey: key}, bk.BookID, vd)
			if aerr != nil || info == nil || !info.Active || !info.Postable {
				return nil, exc(FamCOA, "REVALUATION_ACCOUNT_UNAVAILABLE", "the mapping %s does not resolve to a postable account on %s", key, vd.Format("2006-01-02")).with("mapping_key", key)
			}
			if key == MapFXUnrealizedGain {
				plan.GainAccount = info.Code
			} else {
				plan.LossAccount = info.Code
			}
		}
		if s.Gate != nil {
			if err := s.Gate.CheckPostable(ctx, tenant, bk.LegalEntityID, period, vd, false, "", a.PrincipalID); err != nil {
				return nil, err
			}
		}
		if plan.AutoReverse {
			plan.ReversalDate = vd.AddDate(0, 0, 1)
			plan.ReversalPeriod = FiscalPeriodOf(plan.ReversalDate)
			if s.Gate != nil && plan.ReversalPeriod != period {
				if err := s.Gate.CheckPostable(ctx, tenant, bk.LegalEntityID, plan.ReversalPeriod, plan.ReversalDate, false, "", a.PrincipalID); err != nil {
					return nil, fmt.Errorf("the reversal of the revaluation (%s) cannot be posted: %w", plan.ReversalDate.Format("2006-01-02"), err)
				}
			}
		}
	}
	return s.Repo.WriteRevaluation(ctx, plan)
}
