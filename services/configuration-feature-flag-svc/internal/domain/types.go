// Package domain contains the authoritative domain types for
// configuration-feature-flag-svc.
//
// Both ConfigEntry and FeatureFlag are versioned, effective-dated records:
// no UPDATE/DELETE, ever — a change is always a new row plus an end-dated
// predecessor (see internal/store's Upsert* methods). This mirrors the
// "no soft-delete" doctrine invariant used across this repo, applied here
// on the approved build task's explicit instruction — see context.md §7.
package domain

import (
	"encoding/json"
	"time"
)

// ConfigEntry is one version of a runtime configuration value, scoped to
// an environment and optionally a tenant.
type ConfigEntry struct {
	ConfigID string `json:"config_id"`

	Key string `json:"key"`

	// Value holds the actual config content. json.RawMessage so it is
	// inlined in API responses as JSON, not base64-encoded bytes.
	Value json.RawMessage `json:"value"`

	Environment string `json:"environment"`

	// TenantID nil means this entry is the global default for Environment.
	TenantID *string `json:"tenant_id"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`

	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	CreatedAt            time.Time `json:"created_at"`
}

// FeatureFlag is one version of a feature flag's state, scoped to an
// environment and optionally a tenant.
type FeatureFlag struct {
	FlagID string `json:"flag_id"`

	Key string `json:"key"`

	Enabled bool `json:"enabled"`

	Environment string `json:"environment"`

	// TenantID nil means this flag state is the global default for
	// Environment.
	TenantID *string `json:"tenant_id"`

	// RolloutPercentage is 0-100. Defaults to 100 (fully rolled out) when
	// not supplied on write.
	RolloutPercentage int `json:"rollout_percentage"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`

	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	CreatedAt            time.Time `json:"created_at"`
}

// UpsertConfigEntryParams holds input parameters for writing a new config
// entry version.
type UpsertConfigEntryParams struct {
	Key                  string
	Value                json.RawMessage
	Environment          string
	TenantID             *string
	CreatedByPrincipalID string
}

// UpsertFeatureFlagParams holds input parameters for writing a new
// feature flag version.
type UpsertFeatureFlagParams struct {
	Key                  string
	Enabled              bool
	Environment          string
	TenantID             *string
	RolloutPercentage    int
	CreatedByPrincipalID string
}

// ErrConfigEntryNotFound is returned when no currently-effective config
// entry exists for the requested (key, environment, tenant_id) scope.
var ErrConfigEntryNotFound = errorString("config entry not found")

// ErrFeatureFlagNotFound is returned when no currently-effective feature
// flag exists for the requested (key, environment, tenant_id) scope.
var ErrFeatureFlagNotFound = errorString("feature flag not found")

// ErrStoreUnavailable is returned when the database cannot be reached.
// Callers must fail-closed — treat as unavailable, not as "not found".
var ErrStoreUnavailable = errorString("configuration store unavailable")

type errorString string

func (e errorString) Error() string { return string(e) }
