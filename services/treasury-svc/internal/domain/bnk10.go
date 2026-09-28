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

// ── FXExposureSnapshot (Wave 15) ────────────────────────────────────────────
//
// GetFXExposure/RunFXScenario above remain exactly what they were: live,
// never-persisted composition reads. FXExposureSnapshot is the doc's
// separate, real entity — CalculateFXExposure persists what
// buildExposure (internal/handler/bnk10_handler.go) already computes,
// with the same Calculated->Published->Superseded lifecycle Wave 14 gave
// BNK-08's CashPositionSnapshot. RunFXScenario is UNCHANGED — its
// hypothetical-rate output is never written here, keeping the doc's own
// "scenario outputs remain analytical and separate from approved
// treasury actions" rule structurally true, not just documented.
const (
	FXExposureCalculated = "CALCULATED"
	FXExposurePublished  = "PUBLISHED"
	FXExposureSuperseded = "SUPERSEDED"
)

func CanPublishFXExposure(status string) bool { return status == FXExposureCalculated }
func CanSupersedeFXExposure(status string) bool {
	return status == FXExposureCalculated || status == FXExposurePublished
}

// FXExposureSnapshot is BNK-10's real persisted entity.
type FXExposureSnapshot struct {
	SnapshotID         string    `json:"snapshot_id"`
	TenantID           string    `json:"tenant_id"`
	LegalEntityID      string    `json:"legal_entity_id"`
	ExposureCurrency   string    `json:"exposure_currency"`
	FunctionalCurrency string    `json:"functional_currency"`
	AsOfTimestamp      time.Time `json:"as_of_timestamp"`

	RateUsed  float64   `json:"rate_used"`
	RateAsOf  time.Time `json:"rate_as_of"`
	RateVersion string  `json:"rate_version,omitempty"`
	// NettingScope records what this calculation's scope actually was —
	// see this file's own doc comment: always single-entity today, never
	// silently implying a netting algorithm that doesn't exist.
	NettingScope string `json:"netting_scope"`

	Buckets             []FXExposureBucket `json:"buckets"`
	GrossExposureAmount float64            `json:"gross_exposure_amount"`
	NetExposureAmount   float64            `json:"net_exposure_amount"`

	Status                 string     `json:"status"`
	// EffectiveStatus mirrors BNK-08's own derived-staleness pattern:
	// PUBLISHED becomes "STALE" (never stored) when HasStaleComponent.
	EffectiveStatus        string     `json:"effective_status"`
	HasStaleComponent      bool       `json:"has_stale_component"`
	PublishedByPrincipalID string     `json:"published_by_principal_id,omitempty"`
	PublishedAt            *time.Time `json:"published_at,omitempty"`
	SupersededBy           *string    `json:"superseded_by,omitempty"`

	CalculatedByPrincipalID string    `json:"calculated_by_principal_id"`
	CorrelationID           string    `json:"correlation_id,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
}

// FXExposureCalculation is buildExposure's result, handed to the store to
// persist — same "handler composes, store persists" split Wave 14 uses.
type FXExposureCalculation struct {
	RateUsed      float64
	RateAsOf      time.Time
	RateVersion   string
	HasStaleComponent bool
	Buckets       []FXExposureBucket
	GrossExposureAmount float64
	NetExposureAmount   float64
	AsOfTimestamp time.Time
}

type CalculateFXExposureParams struct {
	TenantID, LegalEntityID, ExposureCurrency, FunctionalCurrency string
	NettingScope                                                  string
	CorrelationID                                                 string
	ActorPrincipalID                                              string
}

type RefreshFXExposureParams struct {
	CalculateFXExposureParams
	PriorSnapshotID string
}

type PublishFXExposureParams struct {
	TenantID, SnapshotID, ActorPrincipalID string
}

type SupersedeFXExposureParams struct {
	TenantID, SnapshotID, NewSnapshotID string
}

var (
	ErrFXExposureSnapshotNotFound  = errorString("fx exposure snapshot not found")
	ErrInvalidFXExposureTransition = errorString("fx exposure snapshot is not in a state that permits this action")
	// ErrFXExposureStaleCannotPublish mirrors
	// ErrCashPositionStaleCannotPublish — a snapshot calculated from a
	// stale FX rate cannot be marked authoritative.
	ErrFXExposureStaleCannotPublish = errorString("fx exposure snapshot was calculated from a stale rate and cannot be published as current")
)
