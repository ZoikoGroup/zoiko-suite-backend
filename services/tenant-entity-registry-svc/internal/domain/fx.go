package domain

// REF-03 Foreign Exchange Rates (ZS-SVC-I-001 s4.13).
//
// Rates are exact decimals: a string at every boundary and math/big inside, never a float. Rate type is mandatory and
// types are never conflated. A derived inverse or cross rate carries the components it came from and the formula, so it
// can be traced to the observations behind it.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// FXRateType is what a rate is for. Types are never conflated: a spot rate is not a closing rate.
type FXRateType string

const (
	FXSpot          FXRateType = "SPOT"
	FXAverage       FXRateType = "AVERAGE"
	FXClosing       FXRateType = "CLOSING"
	FXTax           FXRateType = "TAX"
	FXContractual   FXRateType = "CONTRACTUAL"
	FXOtherGoverned FXRateType = "OTHER_GOVERNED"
)

// Valid reports whether t is a known rate type.
func (t FXRateType) Valid() bool {
	switch t {
	case FXSpot, FXAverage, FXClosing, FXTax, FXContractual, FXOtherGoverned:
		return true
	}
	return false
}

// FXSourceClass is what kind of source produced a rate.
type FXSourceClass string

const (
	FXSourceProvider  FXSourceClass = "PROVIDER"
	FXSourceCentral   FXSourceClass = "CENTRAL_BANK"
	FXSourceTreasury  FXSourceClass = "INTERNAL_TREASURY"
	FXSourceManualExc FXSourceClass = "MANUAL_EXCEPTION"
)

// Valid reports whether c is a known source class.
func (c FXSourceClass) Valid() bool {
	switch c {
	case FXSourceProvider, FXSourceCentral, FXSourceTreasury, FXSourceManualExc:
		return true
	}
	return false
}

// FX observation statuses (s4.13: Observed, Validated, Approved/InRateSet, Superseded).
const (
	FXObserved   = "OBSERVED"
	FXValidated  = "VALIDATED"
	FXApproved   = "APPROVED"
	FXSuperseded = "SUPERSEDED"
)

// FX rate set statuses.
const (
	FXSetApproved   = "APPROVED"
	FXSetClosed     = "CLOSED"
	FXSetSuperseded = "SUPERSEDED"
)

// Rate arithmetic limits: 20 integer digits and 18 decimals, matching the column.
const (
	FXDecimals = 18
	// FXOutlierDeviation is the relative move from the last accepted rate of the same pair and type past which an
	// observation is flagged an outlier and needs independent approval. The spec says outlier checks are server
	// resolved; no policy source exists yet, so this is one constant for every pair.
	FXOutlierDeviation = "0.10"
	// FXDefaultPivot is the currency a cross rate is derived through when none is named.
	FXDefaultPivot = "USD"
	// FXMaxSetMembers bounds one rate set.
	FXMaxSetMembers = 500
)

var rateShape = regexp.MustCompile(`^(0|[1-9][0-9]{0,19})(\.[0-9]{1,18})?$`)
var currencyShape = regexp.MustCompile(`^[A-Z]{3}$`)

// ParseRate reads a positive exact decimal.
func ParseRate(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if !rateShape.MatchString(s) {
		return nil, fmt.Errorf("rate must be a positive decimal with at most 20 integer digits and 18 decimals")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 {
		return nil, fmt.Errorf("rate must be greater than zero")
	}
	return r, nil
}

// FormatRate renders r to at most 18 decimals, rounding half up, without trailing zeros.
func FormatRate(r *big.Rat) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(FXDecimals), nil)
	n := new(big.Int).Mul(r.Num(), scale)
	// round half up: (2n + d) / 2d
	d := r.Denom()
	n.Mul(n, big.NewInt(2))
	n.Add(n, d)
	n.Quo(n, new(big.Int).Mul(d, big.NewInt(2)))
	s := n.String()
	for len(s) <= FXDecimals {
		s = "0" + s
	}
	whole, frac := s[:len(s)-FXDecimals], strings.TrimRight(s[len(s)-FXDecimals:], "0")
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

// CanonicalRate trims a decimal string to its canonical form: no trailing zeros, no trailing point.
func CanonicalRate(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// FXSource is an approved source of rates.
type FXSource struct {
	SourceID      string        `json:"source_id"`
	TenantID      string        `json:"tenant_id"`
	SourceCode    string        `json:"source_code"`
	Name          string        `json:"name"`
	SourceClass   FXSourceClass `json:"source_class"`
	Timezone      string        `json:"timezone"`
	Status        string        `json:"status"`
	RecordVersion int64         `json:"record_version"`
	CreatedBy     string        `json:"created_by"`
	CreatedAt     time.Time     `json:"created_at"`
	WithdrawnBy   string        `json:"withdrawn_by,omitempty"`
	WithdrawnAt   *time.Time    `json:"withdrawn_at,omitempty"`
}

// CreateFXSourceRequest registers a source. Registering it approves it: only approved sources can feed rates.
type CreateFXSourceRequest struct {
	SourceCode    string        `json:"source_code"`
	Name          string        `json:"name"`
	SourceClass   FXSourceClass `json:"source_class"`
	Timezone      string        `json:"timezone,omitempty"`
	CorrelationID string        `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r CreateFXSourceRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.SourceCode) == "" || len(r.SourceCode) > 40:
		return fmt.Errorf("source_code is required (1 to 40 characters)")
	case strings.TrimSpace(r.Name) == "" || len(r.Name) > 200:
		return fmt.Errorf("name is required (1 to 200 characters)")
	case !r.SourceClass.Valid():
		return fmt.Errorf("source_class must be PROVIDER, CENTRAL_BANK, INTERNAL_TREASURY or MANUAL_EXCEPTION")
	case len(r.Timezone) > 64:
		return fmt.Errorf("timezone is too long")
	}
	if r.Timezone != "" {
		if _, err := time.LoadLocation(r.Timezone); err != nil {
			return fmt.Errorf("timezone is not an IANA zone")
		}
	}
	return nil
}

// WithdrawFXSourceRequest withdraws a source: no new rates, existing ones stay.
type WithdrawFXSourceRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r WithdrawFXSourceRequest) Validate() error {
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}

// FXCheck is one validation check run at ingestion, kept as evidence.
type FXCheck struct {
	Check  string `json:"check"`
	Result string `json:"result"` // PASS, FLAGGED
	Detail string `json:"detail,omitempty"`
}

// FXObservation is one rate a source reported.
type FXObservation struct {
	ObservationID         string     `json:"observation_id"`
	TenantID              string     `json:"tenant_id"`
	SourceID              string     `json:"source_id"`
	BaseCurrency          string     `json:"base_currency"`
	QuoteCurrency         string     `json:"quote_currency"`
	RateType              FXRateType `json:"rate_type"`
	Rate                  string     `json:"rate"`
	MarketDate            string     `json:"market_date"`
	ObservedAt            time.Time  `json:"observed_at"`
	ProviderObservationID string     `json:"provider_observation_id,omitempty"`
	PayloadRef            string     `json:"payload_ref"`
	PayloadHash           string     `json:"payload_hash,omitempty"`
	Status                string     `json:"status"`
	Manual                bool       `json:"manual"`
	Outlier               bool       `json:"outlier"`
	NeedsIndependent      bool       `json:"needs_independent_approval"`
	Checks                []FXCheck  `json:"checks"`
	Reason                string     `json:"reason,omitempty"`
	EvidenceRef           string     `json:"evidence_ref,omitempty"`
	IngestedBy            string     `json:"ingested_by"`
	IngestedAt            time.Time  `json:"ingested_at"`
	SupersededByID        string     `json:"superseded_by_id,omitempty"`
	SupersededAt          *time.Time `json:"superseded_at,omitempty"`
}

// IngestFXObservationRequest is IngestFXObservation.
type IngestFXObservationRequest struct {
	SourceID              string     `json:"source_id"`
	BaseCurrency          string     `json:"base_currency"`
	QuoteCurrency         string     `json:"quote_currency"`
	RateType              FXRateType `json:"rate_type"`
	Rate                  string     `json:"rate"`
	MarketDate            string     `json:"market_date"`
	ObservedAt            time.Time  `json:"observed_at"`
	ProviderObservationID string     `json:"provider_observation_id,omitempty"`
	PayloadRef            string     `json:"payload_ref"`
	PayloadHash           string     `json:"payload_hash,omitempty"`
	// Reason and EvidenceRef are required for a MANUAL_EXCEPTION source and optional otherwise.
	Reason        string `json:"reason,omitempty"`
	EvidenceRef   string `json:"evidence_ref,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

var hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks the request against now. Currency existence is the service's check against the registry.
func (r IngestFXObservationRequest) Validate(now time.Time) error {
	switch {
	case strings.TrimSpace(r.SourceID) == "":
		return fmt.Errorf("source_id is required")
	case !currencyShape.MatchString(r.BaseCurrency) || !currencyShape.MatchString(r.QuoteCurrency):
		return fmt.Errorf("base_currency and quote_currency must be three upper-case letters")
	case r.BaseCurrency == r.QuoteCurrency:
		return fmt.Errorf("base_currency and quote_currency must differ")
	case !r.RateType.Valid():
		return fmt.Errorf("rate_type is required: SPOT, AVERAGE, CLOSING, TAX, CONTRACTUAL or OTHER_GOVERNED")
	case strings.TrimSpace(r.PayloadRef) == "":
		return fmt.Errorf("payload_ref is required: a rate is kept with the payload it came from")
	case r.PayloadHash != "" && !hexHash.MatchString(r.PayloadHash):
		return fmt.Errorf("payload_hash must be a lower-case hex SHA-256")
	case r.ObservedAt.IsZero():
		return fmt.Errorf("observed_at is required")
	case r.ObservedAt.After(now.Add(5 * time.Minute)):
		return fmt.Errorf("observed_at is in the future")
	case len(r.ProviderObservationID) > 200:
		return fmt.Errorf("provider_observation_id is too long")
	}
	if _, err := ParseRate(r.Rate); err != nil {
		return err
	}
	md, err := ParseDate(r.MarketDate)
	if err != nil {
		return fmt.Errorf("market_date %v", err)
	}
	if md.After(now.Add(24 * time.Hour)) {
		return fmt.Errorf("market_date is in the future")
	}
	return nil
}

// Fingerprint identifies what an ingestion asks for, so a replay carrying a different rate is refused.
func (r IngestFXObservationRequest) Fingerprint() string {
	rate, _ := ParseRate(r.Rate)
	canon := ""
	if rate != nil {
		canon = FormatRate(rate)
	}
	raw, _ := json.Marshal([]string{r.SourceID, r.BaseCurrency, r.QuoteCurrency, string(r.RateType), canon, r.MarketDate,
		r.ObservedAt.UTC().Format(time.RFC3339Nano), r.ProviderObservationID, r.PayloadHash})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SupersedeFXObservationRequest is SupersedeRateObservation: a corrected observation replaces one not yet in a set.
type SupersedeFXObservationRequest struct {
	Reason string `json:"reason"`
	IngestFXObservationRequest
}

// Validate checks the request.
func (r SupersedeFXObservationRequest) Validate(now time.Time) error {
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("reason is required")
	}
	return r.IngestFXObservationRequest.Validate(now)
}

// FXRateSet is an approved set of rates for a type and market date.
type FXRateSet struct {
	SetID             string          `json:"set_id"`
	TenantID          string          `json:"tenant_id"`
	SetName           string          `json:"set_name"`
	RateType          FXRateType      `json:"rate_type"`
	MarketDate        string          `json:"market_date"`
	SetVersion        int             `json:"set_version"`
	Status            string          `json:"status"`
	SupersedesSetID   string          `json:"supersedes_set_id,omitempty"`
	SupersededBySetID string          `json:"superseded_by_set_id,omitempty"`
	OverrideReason    string          `json:"override_reason,omitempty"`
	EvidenceRef       string          `json:"evidence_ref"`
	ApprovedBy        string          `json:"approved_by"`
	ApprovedAt        time.Time       `json:"approved_at"`
	ClosedBy          string          `json:"closed_by,omitempty"`
	ClosedAt          *time.Time      `json:"closed_at,omitempty"`
	SupersededAt      *time.Time      `json:"superseded_at,omitempty"`
	Members           []FXObservation `json:"members,omitempty"`
}

// ApproveFXRateSetRequest is ApproveFXRateSet: it creates the set already approved.
type ApproveFXRateSetRequest struct {
	SetName        string     `json:"set_name,omitempty"`
	RateType       FXRateType `json:"rate_type"`
	MarketDate     string     `json:"market_date"`
	ObservationIDs []string   `json:"observation_ids"`
	// OverrideReason is required when the set includes an observation that is not VALIDATED: a manual or outlying
	// rate, approved on the approver's recorded say-so.
	OverrideReason string `json:"override_reason,omitempty"`
	EvidenceRef    string `json:"evidence_ref"`
	// SupersedesSetID names the set this one corrects. Closed sets are corrected only this way.
	SupersedesSetID string `json:"supersedes_set_id,omitempty"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r ApproveFXRateSetRequest) Validate() error {
	switch {
	case !r.RateType.Valid():
		return fmt.Errorf("rate_type is required")
	case len(r.SetName) > 40:
		return fmt.Errorf("set_name is too long")
	case strings.TrimSpace(r.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required")
	case len(r.ObservationIDs) == 0:
		return fmt.Errorf("observation_ids is required")
	case len(r.ObservationIDs) > FXMaxSetMembers:
		return fmt.Errorf("a rate set holds at most %d observations", FXMaxSetMembers)
	}
	if _, err := ParseDate(r.MarketDate); err != nil {
		return fmt.Errorf("market_date %v", err)
	}
	seen := map[string]bool{}
	for _, id := range r.ObservationIDs {
		if seen[id] {
			return fmt.Errorf("observation_ids names %s twice", id)
		}
		seen[id] = true
	}
	return nil
}

// CloseFXRateSetRequest is CloseRateSet: certifies a set, after which it is immutable.
type CloseFXRateSetRequest struct {
	EvidenceRef   string `json:"evidence_ref"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r CloseFXRateSetRequest) Validate() error {
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required")
	}
	return nil
}

// FXComponent is one rate a derived rate was built from.
type FXComponent struct {
	ObservationID string `json:"observation_id"`
	BaseCurrency  string `json:"base_currency"`
	QuoteCurrency string `json:"quote_currency"`
	Rate          string `json:"rate"`
	// Direction is DIRECT when the component is used as observed, INVERTED when its reciprocal is used.
	Direction  string `json:"direction"`
	SourceID   string `json:"source_id"`
	SetID      string `json:"set_id"`
	SetVersion int    `json:"set_version"`
	MarketDate string `json:"market_date"`
}

// FXRate is a rate and how it was reached.
type FXRate struct {
	BaseCurrency  string        `json:"base_currency"`
	QuoteCurrency string        `json:"quote_currency"`
	RateType      FXRateType    `json:"rate_type"`
	MarketDate    string        `json:"market_date"`
	Rate          string        `json:"rate"`
	Method        string        `json:"method"` // DIRECT, INVERSE or CROSS
	Formula       string        `json:"formula"`
	Rounding      string        `json:"rounding,omitempty"`
	Exact         string        `json:"exact,omitempty"` // the unrounded fraction, for a derived rate
	Components    []FXComponent `json:"components"`
}

// FX derivation methods.
const (
	FXDirect  = "DIRECT"
	FXInverse = "INVERSE"
	FXCross   = "CROSS"
)

// FXRounding states how a derived rate is rounded; the exact fraction is returned beside it.
const FXRounding = "HALF_UP at 18 decimal places"

// InvertRate is the reciprocal of a positive rate, exactly.
func InvertRate(rate *big.Rat) *big.Rat { return new(big.Rat).Inv(rate) }

// FractionString renders r as numerator/denominator, exactly.
func FractionString(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	return r.Num().String() + "/" + r.Denom().String()
}

// OutlierExceeds reports whether |new/prev - 1| exceeds the threshold, in exact arithmetic.
func OutlierExceeds(prev, next *big.Rat) bool {
	if prev.Sign() == 0 {
		return false
	}
	limit, _ := new(big.Rat).SetString(FXOutlierDeviation)
	dev := new(big.Rat).Quo(new(big.Rat).Sub(next, prev), prev)
	return dev.Abs(dev).Cmp(limit) > 0
}
