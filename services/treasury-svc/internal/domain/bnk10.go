package domain

import "time"

// BNK-10 FX Exposure. No real market-data FX feed exists anywhere in this
// platform — fx_rates is therefore explicit, versioned, admin/caller-
// supplied input, the same honesty pattern as the Audit domain's
// sample_size being explicit auditor input rather than a derived
// formula. Exposure recognition is a live composition read (same pattern
// as EffectiveCashResponse) over the existing AP/AR/obligations clients,
// converted at the latest recorded rate — never a persisted snapshot, so
// there is nothing here for a stale table to silently drift from.
//
// RunFXScenario is pure computation over the SAME composed AR/AP/
// obligations data, using a caller-supplied hypothetical rate instead of
// the latest recorded one. It has no outbound client to
// payment-initiation-adapter-svc, general-ledger-svc or
// intercompany-accounting-svc — not merely unused at runtime, but not
// imported anywhere in this package, so it is structurally incapable of
// ever moving money.

// FXRate is one append-only, versioned rate observation — see migration
// 000005's own reject_fx_rate_mutation trigger. CurrencyPair is
// "{EXPOSURE}/{FUNCTIONAL}", e.g. "EUR/USD" meaning Rate is how many USD
// one EUR is worth.
type FXRate struct {
	RateID                string    `json:"rate_id"`
	TenantID              string    `json:"tenant_id"`
	CurrencyPair          string    `json:"currency_pair"`
	Rate                  float64   `json:"rate"`
	EffectiveAt           time.Time `json:"effective_at"`
	RecordedByPrincipalID string    `json:"recorded_by_principal_id"`
	CreatedAt             time.Time `json:"created_at"`
}

type RecordFXRateParams struct {
	TenantID, CurrencyPair string
	Rate                   float64
	EffectiveAt            time.Time
	RecordedByPrincipalID  string
}

// FXExposureBucket buckets one AP/AR/obligation category's amount into a
// maturity window, converted to the functional currency at whatever rate
// the caller (GetFXExposure vs. RunFXScenario) is using.
type FXExposureBucket struct {
	MaturityBucket           string  `json:"maturity_bucket"`
	Category                 string  `json:"category"`
	ExposureCurrencyAmount   float64 `json:"exposure_currency_amount"`
	FunctionalCurrencyAmount float64 `json:"functional_currency_amount"`
}

// FXExposureResponse is GetFXExposure's live composition result.
// RateIsStale mirrors BNK-08's own freshness discipline: a rate older
// than FXRateStalenessThreshold is flagged, not silently used as if it
// were current.
type FXExposureResponse struct {
	TenantID              string             `json:"tenant_id"`
	LegalEntityID         string             `json:"legal_entity_id"`
	ExposureCurrency      string             `json:"exposure_currency"`
	FunctionalCurrency    string             `json:"functional_currency"`
	RateUsed              float64            `json:"rate_used"`
	RateAsOf              time.Time          `json:"rate_as_of"`
	RateIsStale           bool               `json:"rate_is_stale"`
	Buckets               []FXExposureBucket `json:"buckets"`
	TotalExposureAmount   float64            `json:"total_exposure_currency_amount"`
	TotalFunctionalAmount float64            `json:"total_functional_currency_amount"`
	AsOfTimestamp         time.Time          `json:"as_of_timestamp"`
}

// FXRateStalenessThreshold: a rate not refreshed within this window is
// flagged RateIsStale — same "never present stale as current" doctrine
// as BNK-08's BankBalanceStalenessThreshold, applied to FX rates instead.
const FXRateStalenessThreshold = 24 * time.Hour

type RunFXScenarioRequest struct {
	LegalEntityID      string  `json:"legal_entity_id"`
	ExposureCurrency   string  `json:"exposure_currency"`
	FunctionalCurrency string  `json:"functional_currency"`
	HypotheticalRate   float64 `json:"hypothetical_rate"`
}

// FXScenarioResponse reuses FXExposureResponse's shape for the
// hypothetical-rate result, plus the delta against the CURRENT recognized
// exposure (computed at the latest recorded rate) so a caller can see the
// swing a rate move would produce without it ever being written anywhere.
type FXScenarioResponse struct {
	Current               FXExposureResponse `json:"current"`
	Scenario              FXExposureResponse `json:"scenario"`
	FunctionalAmountDelta float64            `json:"functional_amount_delta"`
}

var (
	ErrFXRateNotFound = errorString("no FX rate has been recorded for this currency pair")
	ErrInvalidFXRate  = errorString("fx rate must be a positive number")
)
