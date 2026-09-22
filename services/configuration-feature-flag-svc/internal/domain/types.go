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

	// CallerTenantID is the tenant the REQUEST came from, which is a different
	// thing from TenantID above — that one is the SCOPE being written, and is
	// nil for the environment-wide default.
	//
	// Both are needed. The scope decides which row is superseded; the caller
	// decides which RLS session the write runs under and which tenant owns the
	// outbox row the write enqueues. Collapsing them breaks a global write:
	// its scope is nil, so an RLS session derived from the scope is unscoped,
	// and the outbox row it enqueues belongs to no tenant and is refused by
	// event_outbox tenant policy — which would make every global write fail
	// at commit, after the version row had already been built.
	//
	// Required. An empty value is refused rather than defaulted: a write that
	// cannot say who made it must not reach an append-only record.
	CallerTenantID string

	// CorrelationID ties the enqueued event back to the request that caused it.
	CorrelationID string
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

	// CallerTenantID and CorrelationID carry the same meaning as on
	// UpsertConfigEntryParams — see that struct for why the caller tenant is
	// separate from the scope being written.
	CallerTenantID string
	CorrelationID  string
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

// ErrScopeRaceConflict is returned when a concurrent writer created the
// currently-effective row for this (key, environment, tenant) scope while this
// request was inserting its own.
//
// The upsert takes FOR UPDATE on the current row, which serialises every write
// to a scope that already has one. The FIRST write to a scope has no row to
// lock, so two concurrent first writes both insert and one loses on the partial
// unique index. That arrived as SQLSTATE 23505, fell through to the generic
// store error, and answered 503 store_unavailable — a lost race reported as a
// dead database, with no hint that retrying would now succeed.
var ErrScopeRaceConflict = errorString("another writer created this scope concurrently")

// ErrCallerTenantMissing is returned when a write reaches the store with no
// caller tenant. It is a programming error rather than a caller mistake — the
// handler resolves the tenant from the gateway-verified header before it gets
// here — but it is refused explicitly rather than defaulted, because the
// alternative is an append-only row and an event that name no owner.
var ErrCallerTenantMissing = errorString("caller tenant is required for a write")

type errorString string

func (e errorString) Error() string { return string(e) }
