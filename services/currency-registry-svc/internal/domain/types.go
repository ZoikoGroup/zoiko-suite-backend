// Package domain defines the authoritative types of currency-registry-svc
// (REF-02).
//
// OWNERSHIP. This service owns Currency, CurrencyLifecycle, MinorUnitMetadata
// and PlatformSupportStatus. It explicitly does NOT own FX rates, accounting
// functional-currency assignment, or monetary amounts, and it performs no
// arithmetic of any kind: minor_unit is an integer exponent and nothing in this
// module uses floating point.
//
// NO CURRENCY VALUE IS HARDCODED. Which ISO 4217 codes exist, their numeric
// codes, names and minor units arrive only through POST /v1/currency-imports
// and live in the database. The only literals here describe SHAPE (three
// letters, three digits, exponent 0..6), never membership.
package domain

import (
	"encoding/json"
	"time"
)

// Status is the global platform-support lifecycle of a currency.
type Status string

const (
	StatusKnown      Status = "KNOWN"
	StatusSupported  Status = "SUPPORTED"
	StatusRestricted Status = "RESTRICTED"
	StatusRetired    Status = "RETIRED"
)

// AllStatuses is the status vocabulary (used for filter validation).
var AllStatuses = []Status{StatusKnown, StatusSupported, StatusRestricted, StatusRetired}

// Valid reports whether s is in the vocabulary.
func (s Status) Valid() bool {
	for _, v := range AllStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// MinorUnit bounds describe the SHAPE of the exponent, not any currency's value.
const (
	MinorUnitMin = 0
	MinorUnitMax = 6
)

// Currency is one registry entry. Alpha and numeric codes are attributes
// (spec section 3: ISO codes are not identity); CurrencyID is the UUIDv7 identity.
type Currency struct {
	CurrencyID      string     `json:"currency_id"`
	AlphaCode       string     `json:"alpha_code"`
	NumericCode     string     `json:"numeric_code"`
	Name            string     `json:"name"`
	FundOrMetalFlag bool       `json:"fund_or_metal_flag"`
	Status          Status     `json:"status"`
	ValidFrom       time.Time  `json:"valid_from"`
	ValidTo         *time.Time `json:"valid_to,omitempty"`
	Version         int64      `json:"version"`

	// LastImportID / LastImportActor identify the import that last introduced
	// or changed this currency. They are the SoD evidence: that actor may not
	// activate it.
	LastImportID    string `json:"last_import_id,omitempty"`
	LastImportActor string `json:"last_import_actor,omitempty"`

	CreatedAt  time.Time `json:"created_at"`
	RecordedAt time.Time `json:"recorded_at"`

	// MinorUnit is the minor-unit version in force at the resolved instant.
	// Populated on reads; never stored on the currency row.
	MinorUnit *MinorUnitVersion `json:"minor_unit,omitempty"`
}

// MinorUnitVersion is one append-only version of a currency's precision.
// ValidTo is DERIVED (next version's valid_from, or the currency's valid_to for
// the last one) and is never stored, so the table can be strictly append-only.
type MinorUnitVersion struct {
	CurrencyID    string     `json:"currency_id"`
	MinorUnit     int        `json:"minor_unit"`
	ValidFrom     time.Time  `json:"valid_from"`
	ValidTo       *time.Time `json:"valid_to,omitempty"`
	SourceVersion string     `json:"source_version"`
	ImportID      string     `json:"import_id,omitempty"`
	EvidenceRef   string     `json:"evidence_ref"`
	RecordedAt    time.Time  `json:"recorded_at"`
}

// StatusHistoryEntry is one append-only lifecycle transition.
type StatusHistoryEntry struct {
	HistoryID        string    `json:"history_id"`
	CurrencyID       string    `json:"currency_id"`
	FromStatus       Status    `json:"from_status"`
	ToStatus         Status    `json:"to_status"`
	Actor            string    `json:"actor"`
	Approver         string    `json:"approver,omitempty"`
	Reason           string    `json:"reason"`
	EffectiveAt      time.Time `json:"effective_at"`
	RecordedAt       time.Time `json:"recorded_at"`
	ResultingVersion int64     `json:"resulting_version"`
	CorrelationID    string    `json:"correlation_id,omitempty"`
}

// ImportStatus is the import lifecycle. Processing is synchronous today, so
// only APPLIED and QUARANTINED are ever persisted; RECEIVED and VALIDATED are
// reserved vocabulary for an async worker (see SPEC_DEVIATIONS.md).
type ImportStatus string

const (
	ImportReceived    ImportStatus = "RECEIVED"
	ImportValidated   ImportStatus = "VALIDATED"
	ImportApplied     ImportStatus = "APPLIED"
	ImportQuarantined ImportStatus = "QUARANTINED"
)

// ImportRow is one source row. MinorUnit is a json.Number so a non-integer
// ("2.5") or non-numeric value survives decoding and quarantines the import
// instead of failing the whole HTTP request, and so no binary floating-point type is involved.
type ImportRow struct {
	AlphaCode       string      `json:"alpha_code"`
	NumericCode     string      `json:"numeric_code"`
	Name            string      `json:"name"`
	MinorUnit       json.Number `json:"minor_unit"`
	FundOrMetalFlag bool        `json:"fund_or_metal_flag"`
}

// RowError is one reason a row (or the file) was refused.
type RowError struct {
	Row     int    `json:"row"` // 0-based index into rows[]; -1 when file-level
	Message string `json:"message"`
}

// ImportSummary counts what an applied import did.
type ImportSummary struct {
	Created           int `json:"created"`
	MinorUnitChanged  int `json:"minor_unit_changed"`
	AttributesChanged int `json:"attributes_changed"`
	Unchanged         int `json:"unchanged"`
}

// Import is the evidence record for one source import.
type Import struct {
	ImportID         string         `json:"import_id"`
	SourceName       string         `json:"source_name"`
	SourceVersion    string         `json:"source_version"`
	ManifestHash     string         `json:"manifest_hash"`
	Status           ImportStatus   `json:"status"`
	RowCount         int            `json:"row_count"`
	QuarantineReason string         `json:"quarantine_reason,omitempty"`
	RowErrors        []RowError     `json:"row_errors,omitempty"`
	Summary          *ImportSummary `json:"summary,omitempty"`
	EffectiveAt      time.Time      `json:"effective_at"`
	Actor            string         `json:"actor"`
	ActorTenantID    string         `json:"actor_tenant_id"`
	Reason           string         `json:"reason"`
	CorrelationID    string         `json:"correlation_id,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	// Rows is the submitted payload, retained as evidence; not echoed on responses.
	Rows []ImportRow `json:"-"`
}

// TenantSupport is a tenant's overlay on a globally SUPPORTED currency.
type TenantSupport struct {
	TenantID   string    `json:"tenant_id"`
	CurrencyID string    `json:"currency_id"`
	AlphaCode  string    `json:"alpha_code"`
	Enabled    bool      `json:"enabled"`
	Version    int64     `json:"version"`
	Reason     string    `json:"reason"`
	Actor      string    `json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Validation reasons returned by the validate endpoint.
const (
	ReasonOK                  = "OK"
	ReasonUnknownCurrency     = "UNKNOWN_CURRENCY"
	ReasonKnownNotActivated   = "STATUS_KNOWN"
	ReasonRestricted          = "STATUS_RESTRICTED"
	ReasonRetired             = "STATUS_RETIRED"
	ReasonNotYetEffective     = "NOT_YET_EFFECTIVE"
	ReasonNotEnabledForTenant = "NOT_ENABLED_FOR_TENANT"
)

// Operations understood by the validate endpoint.
const (
	OperationPost = "post" // a NEW financial posting/payment: needs SUPPORTED
	OperationRead = "read" // historical read: any currency that ever existed
)

// ValidateResult answers "may a caller use this currency for this operation?".
// MinorUnit is a pointer so "unknown" is null rather than a guessed zero.
type ValidateResult struct {
	Code      string `json:"code"`
	Operation string `json:"operation"`
	Supported bool   `json:"supported"`
	MinorUnit *int   `json:"minor_unit"`
	Reason    string `json:"reason"`

	// Pins for consumers that must persist the version they applied.
	CurrencyID      string     `json:"currency_id,omitempty"`
	CurrencyVersion int64      `json:"currency_version,omitempty"`
	Status          Status     `json:"status,omitempty"`
	MinorUnitFrom   *time.Time `json:"minor_unit_valid_from,omitempty"`
}
