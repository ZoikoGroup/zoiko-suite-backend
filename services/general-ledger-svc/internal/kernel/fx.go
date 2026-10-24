package kernel

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Exchange rates (standard §23.1): immutable, typed and sourced. A rate is never edited; a rate keyed in by hand is an override
// that someone else must approve. The rate a posting used is recorded on its lines, so it can always be reproduced.

// Rate types.
var RateTypes = []string{"SPOT", "TRANSACTION", "CLOSING", "AVERAGE", "HISTORICAL", "CONTRACTUAL", "APPROVED_EXTENSION"}

func validRateType(t string) bool {
	for _, x := range RateTypes {
		if x == t {
			return true
		}
	}
	return false
}

// FXPolicy decides which rate type a conversion uses and how old a rate may be. A rate older than the policy allows is refused
// (AK-FX), never silently used: a stale or alternate rate needs an explicit policy that says so.
type FXPolicy struct {
	ID         string
	RateType   string
	MaxAgeDays int
}

// DefaultFXPolicy is used by a rule that names no policy.
const DefaultFXPolicy = "SPOT_ON_DATE"

var fxPolicies = map[string]FXPolicy{
	"SPOT_ON_DATE":        {ID: "SPOT_ON_DATE", RateType: "SPOT", MaxAgeDays: 3},
	"TRANSACTION_ON_DATE": {ID: "TRANSACTION_ON_DATE", RateType: "TRANSACTION", MaxAgeDays: 0},
	"CLOSING_ON_DATE":     {ID: "CLOSING_ON_DATE", RateType: "CLOSING", MaxAgeDays: 0},
	"AVERAGE_FOR_PERIOD":  {ID: "AVERAGE_FOR_PERIOD", RateType: "AVERAGE", MaxAgeDays: 31},
}

// FXPolicyByID returns a built-in policy.
func FXPolicyByID(id string) (FXPolicy, bool) {
	if id == "" {
		id = DefaultFXPolicy
	}
	p, ok := fxPolicies[id]
	return p, ok
}

// FXRate is one stored exchange rate: Rate is how many Quote units one Base unit buys.
type FXRate struct {
	FXRateID           string    `json:"fx_rate_id"`
	TenantID           string    `json:"-"`
	Base               string    `json:"base_currency"`
	Quote              string    `json:"quote_currency"`
	Rate               string    `json:"rate"`
	RateType           string    `json:"rate_type"`
	EffectiveAt        time.Time `json:"effective_at"`
	RateDate           string    `json:"rate_date"`
	Source             string    `json:"source"`
	SourceRateID       string    `json:"source_rate_id"`
	IngestedAt         time.Time `json:"ingested_at"`
	IngestedBy         string    `json:"ingested_by"`
	Override           bool      `json:"override"`
	OverrideReason     string    `json:"override_reason,omitempty"`
	OverrideApprovedBy string    `json:"override_approved_by,omitempty"`
}

// FXOverride is a request to use a rate that did not come from a source.
type FXOverride struct {
	RequestID   string     `json:"request_id"`
	TenantID    string     `json:"-"`
	Base        string     `json:"base_currency"`
	Quote       string     `json:"quote_currency"`
	Rate        string     `json:"rate"`
	RateType    string     `json:"rate_type"`
	EffectiveAt time.Time  `json:"effective_at"`
	RateDate    string     `json:"rate_date"`
	Source      string     `json:"source"`
	SourceRate  string     `json:"source_rate_id"`
	Reason      string     `json:"reason"`
	RequestedBy string     `json:"requested_by"`
	RequestedAt time.Time  `json:"requested_at"`
	Status      string     `json:"status"`
	DecidedBy   string     `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	Note        string     `json:"decision_note,omitempty"`
	FXRateID    string     `json:"fx_rate_id,omitempty"`
}

// FXFilter selects stored rates.
type FXFilter struct {
	Base, Quote, RateType string
	From, To              *time.Time
	Limit                 int
}

// FXRepo is the persistence boundary of exchange rates.
type FXRepo interface {
	// InsertRate stores a rate. The same source reference with the same content returns the stored rate (existing=true); with
	// different content it is an AK-DATA exception.
	InsertRate(ctx context.Context, r *FXRate, out OutEvent) (stored *FXRate, existing bool, err error)
	ListRates(ctx context.Context, tenant string, f FXFilter) ([]FXRate, error)
	// BestRate is the latest rate of the type for the pair with effective_at at or before t (the most recently ingested of equals).
	BestRate(ctx context.Context, tenant, base, quote, rateType string, t time.Time) (*FXRate, error)
	InsertOverride(ctx context.Context, o *FXOverride, out OutEvent) error
	GetOverride(ctx context.Context, tenant, id string) (*FXOverride, error)
	ListOverrides(ctx context.Context, tenant, status string) ([]FXOverride, error)
	// DecideOverride approves (creating the override rate in the same transaction) or rejects a pending request.
	DecideOverride(ctx context.Context, tenant, id, decider string, approve bool, note string, at time.Time, out OutEvent) (*FXOverride, *FXRate, error)
}

// RateResolver implements FXSource over the rate store for one tenant: the policy picks the rate type and the maximum age.
type RateResolver struct {
	Repo   FXRepo
	Tenant string
}

func fixed12(r *big.Rat) string {
	s := r.FloatString(12)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// Basis implements FXSource.
func (rr RateResolver) Basis(ctx context.Context, policyID, from, to string, asOf time.Time) (*FXBasis, error) {
	pol, ok := FXPolicyByID(policyID)
	if !ok {
		return nil, exc(FamFX, "POLICY_UNKNOWN", "exchange-rate policy %q does not exist", policyID)
	}
	day := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, time.UTC)
	cutoff := day.Add(24*time.Hour - time.Nanosecond)
	rate, err := rr.Repo.BestRate(ctx, rr.Tenant, from, to, pol.RateType, cutoff)
	inverse := false
	if errors.Is(err, ErrNotFound) {
		rate, err = rr.Repo.BestRate(ctx, rr.Tenant, to, from, pol.RateType, cutoff)
		inverse = true
	}
	if errors.Is(err, ErrNotFound) {
		return nil, exc(FamFX, "RATE_MISSING", "no %s rate for %s/%s on or before %s", pol.RateType, from, to, day.Format("2006-01-02")).
			with("pair", from+"/"+to).with("rate_type", pol.RateType).with("as_of", day.Format("2006-01-02")).with("policy_id", pol.ID)
	}
	if err != nil {
		return nil, exc(FamExt, "RATE_SOURCE_UNAVAILABLE", "the exchange-rate store could not be read: %v", err)
	}
	rd, _ := time.Parse("2006-01-02", rate.RateDate)
	age := int(day.Sub(rd).Hours() / 24)
	if age > pol.MaxAgeDays {
		return nil, exc(FamFX, "RATE_STALE", "the latest %s rate for %s/%s is dated %s, %d days before %s, and policy %s allows %d", pol.RateType, from, to, rate.RateDate, age, day.Format("2006-01-02"), pol.ID, pol.MaxAgeDays).
			with("rate_date", rate.RateDate).with("max_age_days", pol.MaxAgeDays).with("fx_rate_id", rate.FXRateID)
	}
	val, _ := new(big.Rat).SetString(rate.Rate)
	id := rate.FXRateID
	if inverse {
		val = new(big.Rat).Inv(val)
		id += ":inverse"
	}
	return &FXBasis{Rate: fixed12(val), RateType: rate.RateType, Source: rate.Source, AsOf: rate.RateDate, RateID: id}, nil
}

// ── commands ───────────────────────────────────────────────────────────────────

// FXService is the command side of exchange rates.
type FXService struct {
	Repo  FXRepo
	Clock func() time.Time
}

func (s *FXService) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func checkRate(base, quote, rate, rateType string, at time.Time, source, srcID string) (string, time.Time, error) {
	bad := func(f, why string) error { return exc(FamFX, "RATE_INVALID", "%s: %s", f, why).with("field", f) }
	if !currencyRE.MatchString(base) || !currencyRE.MatchString(quote) || base == quote {
		return "", time.Time{}, bad("base_currency", "two different ISO 4217 codes")
	}
	if !validRateType(rateType) {
		return "", time.Time{}, bad("rate_type", strings.Join(RateTypes, ", "))
	}
	r, ok := ParseDecimal(strings.TrimSpace(rate))
	if !ok || r.Sign() <= 0 {
		return "", time.Time{}, bad("rate", "a positive decimal")
	}
	if new(big.Rat).Mul(r, big.NewRat(1000000000000, 1)).Denom().Cmp(big.NewInt(1)) != 0 || r.Cmp(big.NewRat(1000000000000, 1)) >= 0 {
		return "", time.Time{}, bad("rate", "at most 12 decimals and below 10^12")
	}
	if at.IsZero() {
		return "", time.Time{}, bad("effective_at", "required")
	}
	if strings.TrimSpace(source) == "" || len(source) > 120 || strings.TrimSpace(srcID) == "" || len(srcID) > 200 {
		return "", time.Time{}, bad("source", "the provider and its own reference for the rate are required")
	}
	return fixed12(r), at.UTC(), nil
}

// Ingest stores a sourced rate.
func (s *FXService) Ingest(ctx context.Context, tenant string, a Actor, r FXRate) (*FXRate, bool, error) {
	rate, at, err := checkRate(r.Base, r.Quote, r.Rate, r.RateType, r.EffectiveAt, r.Source, r.SourceRateID)
	if err != nil {
		return nil, false, err
	}
	r.Rate, r.EffectiveAt = rate, at
	if r.RateDate == "" {
		r.RateDate = at.Format("2006-01-02")
	} else if _, perr := time.Parse("2006-01-02", r.RateDate); perr != nil {
		return nil, false, exc(FamFX, "RATE_INVALID", "rate_date: a date, YYYY-MM-DD").with("field", "rate_date")
	}
	r.FXRateID, r.TenantID, r.IngestedAt, r.IngestedBy, r.Override, r.OverrideReason, r.OverrideApprovedBy = uuid.NewString(), tenant, s.now(), a.PrincipalID, false, "", ""
	return s.Repo.InsertRate(ctx, &r, OutEvent{Fact: "fx.rate-ingested", AggregateType: "fx-rate", AggregateID: r.FXRateID, Actor: a.PrincipalID, CorrelationID: a.CorrelationID,
		Data: map[string]any{"fx_rate_id": r.FXRateID, "pair": r.Base + "/" + r.Quote, "rate": r.Rate, "rate_type": r.RateType, "rate_date": r.RateDate, "source": r.Source}})
}

// RequestOverride asks for a hand-keyed rate. It is not a rate until someone else approves it.
func (s *FXService) RequestOverride(ctx context.Context, tenant string, a Actor, o FXOverride) (*FXOverride, error) {
	rate, at, err := checkRate(o.Base, o.Quote, o.Rate, o.RateType, o.EffectiveAt, "OVERRIDE", "pending")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(o.Reason) == "" {
		return nil, exc(FamFX, "OVERRIDE_REASON_REQUIRED", "a rate override needs its justification")
	}
	o.Rate, o.EffectiveAt = rate, at
	if o.RateDate == "" {
		o.RateDate = at.Format("2006-01-02")
	}
	o.RequestID, o.TenantID, o.Source, o.RequestedBy, o.RequestedAt, o.Status = uuid.NewString(), tenant, "OVERRIDE", a.PrincipalID, s.now(), "PENDING"
	o.SourceRate = "override-" + o.RequestID
	o.DecidedBy, o.DecidedAt, o.Note, o.FXRateID = "", nil, "", ""
	if err := s.Repo.InsertOverride(ctx, &o, OutEvent{Fact: "fx.override-requested", AggregateType: "fx-override", AggregateID: o.RequestID, Actor: a.PrincipalID, CorrelationID: a.CorrelationID,
		Data: map[string]any{"request_id": o.RequestID, "pair": o.Base + "/" + o.Quote, "rate": o.Rate, "reason": o.Reason}}); err != nil {
		return nil, err
	}
	return &o, nil
}

// Decide approves or rejects a pending override. The approver is never the requester.
func (s *FXService) Decide(ctx context.Context, tenant string, a Actor, id string, approve bool, note string) (*FXOverride, *FXRate, error) {
	if !approve && strings.TrimSpace(note) == "" {
		return nil, nil, exc(FamFX, "OVERRIDE_REASON_REQUIRED", "a rejection needs its reason")
	}
	fact := "fx.override-rejected"
	if approve {
		fact = "fx.override-approved"
	}
	return s.Repo.DecideOverride(ctx, tenant, id, a.PrincipalID, approve, strings.TrimSpace(note), s.now(), OutEvent{Fact: fact, AggregateType: "fx-override", AggregateID: id, Actor: a.PrincipalID,
		CorrelationID: a.CorrelationID, Data: map[string]any{"request_id": id}})
}
