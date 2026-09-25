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
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
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

// ErrChangeNotFound is returned when a governed change (config_changes row or
// emergency_changes row) with the requested id does not exist.
var ErrChangeNotFound = errorString("change not found")

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

// ── ZS-SVC-AA-001 domain ─────────────────────────────────────────────────────
//
// Everything below is the AA-001 compliance surface added by the audit work:
// the key registry (config definitions), the immutable snapshot the read path
// is pinned to (INV-12), the governed-change machinery (kill switches, change
// sets, break-glass changes) and the attestation/drift pair. The types mirror
// migrations 000004–000008 column for column; when the schema changes the
// migration comes first and this file follows.

// ── AA-001 errors ─────────────────────────────────────────────────────────────
//
// Every refusal the AA-001 surface can produce has a sentinel here carrying its
// documented wire code, so the handler can map errors.Is(err, domain.ErrX) to
// the exact string openapi.yaml documents. A code emitted in one place and
// documented in another is exactly what scripts/audit.sh's "every emitted
// error code is in openapi" check exists to catch.

// codedError is a sentinel error carrying its ZS-SVC-AA-001 wire code.
type codedError struct {
	code string
	msg  string
}

func (e *codedError) Error() string { return e.msg }
func (e *codedError) Code() string  { return e.code }

func newCodedError(code, msg string) error { return &codedError{code: code, msg: msg} }

// ErrorCode returns the documented wire code carried by err, or "" when err is
// not an AA-001 sentinel — store failures and the pre-existing refusals are
// still mapped at the emit site.
func ErrorCode(err error) string {
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	return ""
}

// AA-001 refusals, code-carrying so the handler never invents a string. Each is
// documented in openapi.yaml responses before it is emitted.
var (
	// INV-05: a write gate. Undeclared keys are refused, not created.
	ErrKeyNotRegistered = newCodedError("key_not_registered",
		"no published definition exists for this key")
	// NP-03: a boolean key may not accept a string.
	ErrTypeMismatch = newCodedError("type_mismatch",
		"value does not match the definition's declared value_type")
	// INV-08: the definition's allowed_scopes is an allowlist, not a hint.
	ErrScopeNotAllowed = newCodedError("scope_not_allowed",
		"this key's definition does not allow the requested override scope")
	// A concurrent override at the same layer superseded this one.
	ErrOverrideConflict = newCodedError("override_conflict",
		"the requested override conflicts with one already in force at this layer")
	// NP-03: beyond type, the definition's validation rules.
	ErrValueConstraintFailed = newCodedError("value_constraint_failed",
		"value violates the definition's validation constraints")
	// INV-04: published definition versions are immutable.
	ErrVersionImmutable = newCodedError("version_immutable",
		"published definition versions are immutable — publish a new version instead")
	// INV-22 / OD-04: the imprint is past its freshness deadline.
	ErrSnapshotStale = newCodedError("snapshot_stale",
		"the requested snapshot is past its freshness deadline")
	// A digest mismatch in stored content — corruption, not staleness.
	ErrSnapshotInvalid = newCodedError("snapshot_invalid",
		"snapshot digest does not match its content")
	// INV-22: epochs advance, never rewind.
	ErrSnapshotRollbackRejected = newCodedError("snapshot_rollback_rejected",
		"rollback to this snapshot is rejected — the epoch must advance, never rewind")
	// §6.2: eligibility filters run before any percentage is applied.
	ErrContextIncomplete = newCodedError("context_incomplete",
		"the supplied evaluation context is missing attributes this key requires")
	// INV-19: regulated/rights-affecting outcomes are not randomized.
	ErrTargetingNotPermitted = newCodedError("targeting_not_permitted",
		"the requested targeting is not permitted for this flag")
	ErrEntitlementDenied = newCodedError("entitlement_denied",
		"the subject holds no entitlement for this configuration")
	ErrPolicyOrPrivacyDenied = newCodedError("policy_or_privacy_denied",
		"policy or privacy evaluation denied this resolution")
	ErrEnvironmentBoundaryViolation = newCodedError("environment_boundary_violation",
		"the requested scope crosses an environment boundary")
	// INV-09: SECRET_REFERENCE_ONLY keys accept a reference, never material.
	ErrSecretValueProhibited = newCodedError("secret_value_prohibited",
		"SECRET_REFERENCE_ONLY keys accept a secret reference (secret://...), never material")
	// TC-04: C2/C3 classes bind an approval before activation.
	ErrChangeApprovalRequired = newCodedError("change_approval_required",
		"this change class requires an approval before activation")
	// NP-42: break-glass is a small allowlist, not arbitrary keys.
	ErrEmergencyScopeDenied = newCodedError("emergency_scope_denied",
		"this key is not in the break-glass allowlist")
	// TC-08: observed and desired disagree.
	ErrDriftDetected = newCodedError("drift_detected",
		"the runtime's observed snapshot differs from the desired one")
	ErrConsumerIncompatible = newCodedError("consumer_incompatible",
		"the consumer's observed version is incompatible with this snapshot")
	// INV-28: no authoritative value was served; the fallback policy governs.
	ErrSafeFallbackActive = newCodedError("safe_fallback_active",
		"the definition's fallback policy is in force — no authoritative value was served")
	// INV-21 / NP-20: a retired key is tombstoned, not reusable.
	ErrFlagKeyRetired = newCodedError("flag_key_retired",
		"this flag key is retired and cannot be reused")
	// INV-12: fail closed — no snapshot, no resolution.
	ErrNoAttestedSnapshot = newCodedError("no_attested_snapshot",
		"no snapshot exists for this environment — refusing rather than guessing")
	// INV-15 / NP-43: break-glass without a bound is not break-glass.
	ErrEmergencyChangeNoExpiry = newCodedError("emergency_change_no_expiry",
		"an emergency change must declare an expiry")
)

// ── Key registry: config definitions ──────────────────────────────────────────

// Value types the registry understands (config_definitions CHECK). Are kept as
// strings — the DB owns the enum, Go only reads and validates against it.
const (
	ValueTypeBoolean    = "BOOLEAN"
	ValueTypeInteger    = "INTEGER"
	ValueTypeDecimal    = "DECIMAL"
	ValueTypeString     = "STRING"
	ValueTypeEnum       = "ENUM"
	ValueTypeDuration   = "DURATION"
	ValueTypeURI        = "URI"
	ValueTypeCIDRSet    = "CIDR_SET"
	ValueTypeStringSet  = "STRING_SET"
	ValueTypeStructured = "STRUCTURED"
)

// Safety classes (Table 11, config_definitions CHECK): S0 cosmetic, S1
// operational low risk, S2 material tenant behavior, S3 security/privacy/
// financial/regulated adjacent.
const (
	SafetyS0 = "S0"
	SafetyS1 = "S1"
	SafetyS2 = "S2"
	SafetyS3 = "S3"
)

// Fallback policies (INV-28, config_definitions CHECK): what a consumer may do
// when the control plane is unreachable or the value is stale.
const (
	FallbackUseCachedWithMaxAge = "USE_CACHED_WITH_MAX_AGE"
	FallbackSafeDefault         = "SAFE_DEFAULT"
	FallbackBlock               = "BLOCK"
	FallbackDegrade             = "DEGRADE"
)

// Sensitivities (config_definitions CHECK). SECRET_REFERENCE_ONLY is the
// INV-09 guardrail: the stored value is a secret reference, never material.
const (
	SensitivityPublicConfig        = "PUBLIC_CONFIG"
	SensitivityInternal            = "INTERNAL"
	SensitivityRestrictedMetadata  = "RESTRICTED_METADATA"
	SensitivitySecretReferenceOnly = "SECRET_REFERENCE_ONLY"
)

// Effective models (config_definitions CHECK).
const (
	EffectiveModelImmediate = "IMMEDIATE"
	EffectiveModelScheduled = "SCHEDULED"
	EffectiveModelPeriod    = "PERIOD"
)

// Definition lifecycle (Table 7, config_definitions CHECK).
const (
	LifecycleDraft      = "DRAFT"
	LifecycleReview     = "REVIEW"
	LifecyclePublished  = "PUBLISHED"
	LifecycleDeprecated = "DEPRECATED"
	LifecycleRetired    = "RETIRED"
)

// Override scope names (INV-08, def.allowed_scopes and the precedence layers
// of INV-07). Storage today is ENVIRONMENT + TENANT; the other three layers
// are named here so the precedence chain and the write gate are complete even
// where storage has not landed yet.
const (
	ScopeEnvironment    = "ENVIRONMENT"
	ScopeTenant         = "TENANT"
	LayerUserPreference = "USER_PREFERENCE"
	LayerOrgUnit        = "ORG_UNIT"
	LayerService        = "SERVICE"
)

// Flag classes (Table 16, config_definitions CHECK; NULL for config keys and
// for pre-registry flag keys the backfill could not classify).
const (
	FlagClassRelease       = "RELEASE"
	FlagClassOpsKillSwitch = "OPS_KILL_SWITCH"
	FlagClassMigration     = "MIGRATION"
	FlagClassExperiment    = "EXPERIMENT"
	FlagClassCompatibility = "COMPATIBILITY"
	FlagClassPermanent     = "PERMANENT"
)

// PrecedenceOrder is INV-07's five-layer chain, highest first. Evaluation
// walks it from the top; the first layer that has a value wins.
var PrecedenceOrder = []string{LayerUserPreference, LayerOrgUnit, ScopeTenant, LayerService, ScopeEnvironment}

// IsTemporaryFlagClass reports whether a flag class demands an explicit
// retirement deadline (INV-20/NP-19). PERMANENT — the explicit "no deadline"
// class — is the only non-temporary class.
func IsTemporaryFlagClass(flagClass string) bool {
	switch flagClass {
	case FlagClassRelease, FlagClassOpsKillSwitch, FlagClassMigration,
		FlagClassExperiment, FlagClassCompatibility:
		return true
	}
	return false
}

// ConfigDefinition is the working declaration for one configuration key
// (Table 11). It is editable while unpublished; the instant it is published
// the store copies it, immutably, into a ConfigDefinitionVersion and every
// resolution reads the versions from then on (INV-04/05/06).
type ConfigDefinition struct {
	DefinitionID       string          `json:"definition_id"`
	Key                string          `json:"key"`
	Owner              string          `json:"owner"`
	ValueType          string          `json:"value_type"`
	SafetyClass        string          `json:"safety_class"`
	AllowedScopes      []string        `json:"allowed_scopes"` // ordered, INV-08
	DefaultValue       json.RawMessage `json:"default_value"`
	FallbackPolicy     string          `json:"fallback_policy"`
	Sensitivity        string          `json:"sensitivity"`
	Validation         json.RawMessage `json:"validation"`
	EffectiveModel     string          `json:"effective_model"`
	Lifecycle          string          `json:"lifecycle"`
	Deprecation        json.RawMessage `json:"deprecation"`
	FlagClass          *string         `json:"flag_class"`
	RetirementDeadline *time.Time      `json:"retirement_deadline"`

	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	UpdatedByPrincipalID string    `json:"updated_by_principal_id"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// ConfigDefinitionVersion is one immutable published snapshot of a definition
// (INV-04: Published ConfigVersion objects are immutable). No UPDATE/DELETE
// ever touches this record, mirroring the append-only value tables.
type ConfigDefinitionVersion struct {
	VersionID              string          `json:"version_id"`
	DefinitionID           string          `json:"definition_id"`
	Version                int             `json:"version"`
	Digest                 string          `json:"digest"`
	Definition             json.RawMessage `json:"definition"`
	Lifecycle              string          `json:"lifecycle"`
	PublishedByPrincipalID string          `json:"published_by_principal_id"`
	PublishedAt            time.Time       `json:"published_at"`
}

// CreateDefinitionParams carries a POST /v1/config/definitions write.
type CreateDefinitionParams struct {
	Key                string
	Owner              string
	ValueType          string
	SafetyClass        string
	AllowedScopes      []string
	DefaultValue       json.RawMessage
	FallbackPolicy     string
	Sensitivity        string
	Validation         json.RawMessage
	EffectiveModel     string
	FlagClass          *string
	RetirementDeadline *time.Time
	ActorPrincipalID   string
	CorrelationID      string
}

// PublishDefinitionParams carries a definition publish/deprecate/retire acting
// on a working row; Lifecycle is one of PUBLISHED / DEPRECATED / RETIRED.
type PublishDefinitionParams struct {
	DefinitionID     string
	Lifecycle        string
	ActorPrincipalID string
	CorrelationID    string
}

// ── Write-gate validation helpers ─────────────────────────────────────────────
//
// These live in the domain (not the handler or the store) because every write
// path shares them — the ordinary upsert, the definition-gated upsert, the
// override endpoint and the emergency change path all reach the same answers.

// SecretReferencePrefix is the only material this service ever stores for a
// SECRET_REFERENCE_ONLY key: a reference, never the secret itself (INV-09).
const SecretReferencePrefix = "secret://"

// IsSecretReference reports whether value is a secret *reference* rather than
// material. A non-string, or a string that does not name the secret namespace,
// is material from this service's point of view and is refused.
func IsSecretReference(value json.RawMessage) bool {
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return false
	}
	return strings.HasPrefix(s, SecretReferencePrefix)
}

// ValidateValueAgainstType checks the JSON shape of value against one declared
// value_type (NP-03). DECIMAL accepts any JSON number; INTEGER additionally
// refuses anything with a fraction or exponent, and any integer that overflows
// int64 — a decimal silently coerced to an integer is how rounding bugs start.
func ValidateValueAgainstType(value json.RawMessage, valueType string) error {
	var v any
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return ErrTypeMismatch
	}
	switch valueType {
	case ValueTypeBoolean:
		if _, ok := v.(bool); !ok {
			return ErrTypeMismatch
		}
	case ValueTypeInteger:
		n, ok := v.(json.Number)
		if !ok {
			return ErrTypeMismatch
		}
		if strings.ContainsAny(string(n), ".eE") {
			return ErrTypeMismatch
		}
		if _, err := strconv.ParseInt(string(n), 10, 64); err != nil {
			return ErrTypeMismatch
		}
	case ValueTypeDecimal:
		if _, ok := v.(json.Number); !ok {
			return ErrTypeMismatch
		}
	case ValueTypeString, ValueTypeEnum, ValueTypeURI, ValueTypeDuration:
		if _, ok := v.(string); !ok {
			return ErrTypeMismatch
		}
	case ValueTypeCIDRSet, ValueTypeStringSet:
		arr, ok := v.([]any)
		if !ok {
			return ErrTypeMismatch
		}
		for _, el := range arr {
			if _, ok := el.(string); !ok {
				return ErrTypeMismatch
			}
		}
	case ValueTypeStructured:
		// any JSON value is acceptable, including null
	default:
		return ErrTypeMismatch
	}
	return nil
}

// ValidateValue checks a submitted value against a definition end to end: the
// key must be registered (INV-05), a SECRET_REFERENCE_ONLY key must hold a
// reference (INV-09), the value must match the declared type, and an ENUM must
// name one of the enum options carried in the definition's validation schema.
func ValidateValue(value json.RawMessage, def *ConfigDefinition) error {
	if def == nil {
		return ErrKeyNotRegistered
	}
	if def.Sensitivity == SensitivitySecretReferenceOnly && !IsSecretReference(value) {
		return ErrSecretValueProhibited
	}
	if err := ValidateValueAgainstType(value, def.ValueType); err != nil {
		return err
	}
	if def.ValueType == ValueTypeEnum && len(def.Validation) > 0 {
		var vsc struct {
			Enum []string `json:"enum"`
		}
		if json.Unmarshal(def.Validation, &vsc) == nil && len(vsc.Enum) > 0 {
			var s string
			if json.Unmarshal(value, &s) == nil {
				for _, e := range vsc.Enum {
					if e == s {
						return nil
					}
				}
				return ErrValueConstraintFailed
			}
		}
	}
	return nil
}

// ValidateScope admits the override only when layer is in the definition's
// allowed_scopes allowlist (INV-08). A value never silently overrides a scope
// the definition has not declared.
func ValidateScope(def *ConfigDefinition, layer string) error {
	if def == nil {
		return ErrKeyNotRegistered
	}
	for _, s := range def.AllowedScopes {
		if s == layer {
			return nil
		}
	}
	return ErrScopeNotAllowed
}

// ── Snapshots (INV-12, Table 6) ───────────────────────────────────────────────

// ConfigSnapshotEpoch is one environment's monotonic epoch counter. The mint
// bumps it via INSERT ... ON CONFLICT DO UPDATE, whose row lock serializes
// concurrent mints per environment (INV-22 — epochs never rewind).
type ConfigSnapshotEpoch struct {
	Environment  string    `json:"environment"`
	CurrentEpoch int64     `json:"current_epoch"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ConfigSnapshot is one immutable, epoch-dated imprint of an environment's
// effective configuration (Table 6). content is the manifest keyed
// `key|tenant_id`; digest is md5 of its canonical serialization; read paths
// serve content, never the live admin tables they were minted from (INV-12).
type ConfigSnapshot struct {
	SnapshotID           string          `json:"snapshot_id"`
	Environment          string          `json:"environment"`
	Epoch                int64           `json:"epoch"`
	Digest               string          `json:"digest"`
	Content              json.RawMessage `json:"content"`
	IssuedAt             time.Time       `json:"issued_at"`
	FreshnessDeadline    time.Time       `json:"freshness_deadline"`
	CreatedByPrincipalID string          `json:"created_by_principal_id"`
}

// MintedSnapshot is what the store returns from a mint inside a write
// transaction — the metadata the write's event and the response need, without
// re-reading the whole content the caller already holds.
type MintedSnapshot struct {
	SnapshotID        string    `json:"snapshot_id"`
	Environment       string    `json:"environment"`
	Epoch             int64     `json:"epoch"`
	Digest            string    `json:"digest"`
	IssuedAt          time.Time `json:"issued_at"`
	FreshnessDeadline time.Time `json:"freshness_deadline"`
}

// ResolvedConfigSnapshot is Table 6 — a snapshot plus the per-key outcome of
// resolving it against a scope; what POST /v1/config/resolve returns. It is
// built entirely from stored content, never from live admin rows.
type ResolvedConfigSnapshot struct {
	SnapshotID        string          `json:"snapshot_id"`
	Environment       string          `json:"environment"`
	Epoch             int64           `json:"epoch"`
	Digest            string          `json:"digest"`
	IssuedAt          time.Time       `json:"issued_at"`
	FreshnessDeadline time.Time       `json:"freshness_deadline"`
	Values            []ResolvedValue `json:"values"`
}

// ResolvedValue is one key's resolution within a pinned snapshot (Table 14).
// Outcome is what the consumer may act on; the safety class and fallback
// policy are echoed so a consumer applying INV-28 semantics does not need a
// second lookup.
type ResolvedValue struct {
	Key            string          `json:"key"`
	Environment    string          `json:"environment"`
	TenantID       *string         `json:"tenant_id,omitempty"`
	Value          json.RawMessage `json:"value,omitempty"`
	Outcome        string          `json:"outcome"`
	Reason         string          `json:"reason"`
	Layer          string          `json:"layer"`
	EffectiveFrom  *time.Time      `json:"effective_from,omitempty"`
	SafetyClass    string          `json:"safety_class,omitempty"`
	FallbackPolicy string          `json:"fallback_policy,omitempty"`
}

// Resolution outcomes and reasons (Table 14).
const (
	OutcomeValue         = "VALUE"
	OutcomeSafeDefault   = "SAFE_DEFAULT"
	OutcomeBlocked       = "BLOCKED"
	OutcomeIndeterminate = "INDETERMINATE"
)

const (
	ReasonDefault       = "DEFAULT"
	ReasonBaseline      = "BASELINE"
	ReasonOverride      = "OVERRIDE"
	ReasonSafeDefault   = "SAFE_DEFAULT"
	ReasonBlocked       = "BLOCKED"
	ReasonIndeterminate = "INDETERMINATE"
)

// Manifest kinds — the discriminator on each entry built by
// config_environment_manifest() in migration 000008.
const (
	ManifestKindConfig = "config"
	ManifestKindFlag   = "flag"
)

// ManifestKey builds the manifest's composite key: `key|tenant_id`, with '*'
// standing for the environment-wide default. It must never disagree with the
// SQL function, so the expression is reproduced here verbatim.
func ManifestKey(key string, tenantID *string) string {
	if tenantID == nil || *tenantID == "" {
		return key + "|*"
	}
	return key + "|" + *tenantID
}

// ManifestEntry is one element of a snapshot's content JSONB, discriminated by
// Kind. Flag entries additionally carry the CURRENT release plan and the
// active kill switch, embedded so evaluation is fully pinned to the snapshot
// (a later plan edit or switch flip can never rewrite what a snapshot said).
type ManifestEntry struct {
	Key                  string               `json:"key"`
	Kind                 string               `json:"kind"`
	Value                json.RawMessage      `json:"value,omitempty"`
	ConfigID             string               `json:"config_id,omitempty"`
	FlagID               string               `json:"flag_id,omitempty"`
	Enabled              *bool                `json:"enabled,omitempty"`
	RolloutPercentage    *int                 `json:"rollout_percentage,omitempty"`
	Environment          string               `json:"environment"`
	TenantID             *string              `json:"tenant_id,omitempty"`
	EffectiveFrom        time.Time            `json:"effective_from"`
	CreatedByPrincipalID string               `json:"created_by_principal_id"`
	ReleasePlan          *ReleasePlanManifest `json:"release_plan,omitempty"`
	KillSwitch           *KillSwitchManifest  `json:"kill_switch,omitempty"`
}

// ReleasePlanManifest is the snapshot-embedded view of a flag's current
// release plan. Lighter than the stored row on purpose: the evaluation needs
// the strategy, the salt and the immutable targeting rules; it does not need
// publication bookkeeping.
type ReleasePlanManifest struct {
	ReleasePlanID  string          `json:"release_plan_id"`
	Version        int             `json:"version"`
	Strategy       string          `json:"strategy"`
	Salt           string          `json:"salt"`
	BucketCount    int             `json:"bucket_count"`
	TargetingHash  string          `json:"targeting_hash"`
	TargetingRules json.RawMessage `json:"targeting_rules"`
}

// KillSwitchManifest is the snapshot-embedded view of the active kill switch
// for a flag, if one is in force at mint time.
type KillSwitchManifest struct {
	KillSwitchID string    `json:"kill_switch_id"`
	SafeBehavior string    `json:"safe_behavior"`
	Reason       string    `json:"reason"`
	IncidentID   string    `json:"incident_id,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// ── Kill switches (INV-16, NP-49) ─────────────────────────────────────────────

// Kill switch safe behaviors (config CHECK): a switch may only narrow the
// path — disable it, or degrade it — never broaden it.
const (
	SafeBehaviorDisable     = "DISABLE"
	SafeBehaviorDegradeOnly = "DEGRADE_ONLY"
)

// KillSwitch is a predeclared, narrow, temporary disable/degrade signal for a
// flag key. expires_at is mandatory (no permanent kill switch); expired_at is
// the append-only "ended" marker, so the table keeps full history.
type KillSwitch struct {
	KillSwitchID         string     `json:"kill_switch_id"`
	FlagKey              string     `json:"flag_key"`
	Environment          string     `json:"environment"`
	TenantID             *string    `json:"tenant_id,omitempty"`
	Reason               string     `json:"reason"`
	IncidentID           string     `json:"incident_id,omitempty"`
	SafeBehavior         string     `json:"safe_behavior"`
	ExpiresAt            time.Time  `json:"expires_at"`
	ExpiredAt            *time.Time `json:"expired_at,omitempty"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	CreatedAt            time.Time  `json:"created_at"`
}

// CreateKillSwitchParams carries a kill-switch write.
type CreateKillSwitchParams struct {
	FlagKey           string
	Environment       string
	TenantID          *string
	Reason            string
	IncidentID        *string
	SafeBehavior      string
	ExpiresAt         time.Time
	CallerTenantID    string
	ActorPrincipalID  string
	CorrelationID     string
}

// ── Governed change sets (INV-14, Table 7, Table 21) ─────────────────────────

// Change classes (Table 21): C0 cosmetic, C1 operational, C2 material, C3
// critical. C2/C3 bind an approval before activation (TC-04).
const (
	ChangeClassC0 = "C0"
	ChangeClassC1 = "C1"
	ChangeClassC2 = "C2"
	ChangeClassC3 = "C3"
)

// Change lifecycle (config_changes CHECK). VERIFIED / FAILED / ROLLED_BACK are
// terminal.
const (
	ChangeStatusProposed   = "PROPOSED"
	ChangeStatusValidated  = "VALIDATED"
	ChangeStatusApproved   = "APPROVED"
	ChangeStatusScheduled  = "SCHEDULED"
	ChangeStatusApplying   = "APPLYING"
	ChangeStatusVerified   = "VERIFIED"
	ChangeStatusFailed     = "FAILED"
	ChangeStatusRolledBack = "ROLLED_BACK"
)

// Part kinds inside a ChangeSet.
const (
	PartKindConfig = "config"
	PartKindFlag   = "flag"
)

// ChangePart is one mutation inside a ChangeSet's ordered parts array. A part
// names its expected-before hash when the author wants the activation to fail
// if the live state moved on before it ran.
type ChangePart struct {
	Kind               string          `json:"kind"`
	Key                string          `json:"key"`
	Scope              ChangePartScope `json:"scope"`
	NewValue           json.RawMessage `json:"new_value,omitempty"`
	NewEnabled         *bool           `json:"new_enabled,omitempty"`
	RolloutPercentage  *int            `json:"rollout_percentage,omitempty"`
	ExpectedBeforeHash *string         `json:"expected_before_hash,omitempty"`
}

// ChangePartScope is the (environment, tenant) tuple a part targets.
type ChangePartScope struct {
	Environment string  `json:"environment"`
	TenantID    *string `json:"tenant_id,omitempty"`
}

// ChangeApproval is the approval binding a C2/C3 change carries.
type ChangeApproval struct {
	Approved      bool      `json:"approved"`
	ByPrincipalID string    `json:"by_principal_id"`
	ApprovedAt    time.Time `json:"approved_at"`
	WFCReference  *string   `json:"wfc_reference,omitempty"`
}

// ConfigChange is an atomic ChangeSet with class, before/after snapshot links,
// approval binding and the Table 7 lifecycle. The snapshot links make "what
// changed, and to what" reproducible forever (TC-01, NP-59).
type ConfigChange struct {
	ChangeID             string          `json:"change_id"`
	ChangeClass          string          `json:"change_class"`
	Environment          string          `json:"environment"`
	TenantID             *string         `json:"tenant_id,omitempty"`
	BeforeSnapshotID     *string         `json:"before_snapshot_id,omitempty"`
	ProposedSnapshotID   *string         `json:"proposed_snapshot_id,omitempty"`
	Parts                json.RawMessage `json:"parts"`
	Status               string          `json:"status"`
	Approval             json.RawMessage `json:"approval,omitempty"`
	ApprovalRequired     bool            `json:"approval_required"`
	PlannedEffectiveAt   *time.Time      `json:"planned_effective_at,omitempty"`
	ActivatedAt          *time.Time      `json:"activated_at,omitempty"`
	VerifiedAt           *time.Time      `json:"verified_at,omitempty"`
	RollbackChangeID     *string         `json:"rollback_change_id,omitempty"`
	CreatedByPrincipalID string          `json:"created_by_principal_id"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

// CreateChangeParams carries a POST /v1/config/changes write.
type CreateChangeParams struct {
	ChangeClass        string
	Environment        string
	TenantID           *string
	Parts              []ChangePart
	ApprovalRequired   bool
	Approval           ChangeApproval
	PlannedEffectiveAt *time.Time
	RollbackChangeID   *string
	CallerTenantID     string
	ActorPrincipalID   string
	CorrelationID      string
}

// ── Emergency (break-glass) changes (INV-15, NP-42/43/44) ────────────────────

// Emergency change lifecycle (emergency_changes CHECK).
const (
	EmergencyStatusOpen                 = "OPEN"
	EmergencyStatusActive               = "ACTIVE"
	EmergencyStatusExpired              = "EXPIRED"
	EmergencyStatusRetrospectivePending = "RETROSPECTIVE_PENDING"
	EmergencyStatusClosed               = "CLOSED"
)

// EmergencyChange is a time-boxed break-glass mutation. Expiry is mandatory;
// the background sweep flips an EXPIRED change and reverts it, recording the
// reversion in reverted_to_prior so evidence of the failure is retained
// (NP-44/47).
type EmergencyChange struct {
	EmergencyChangeID      string          `json:"emergency_change_id"`
	Key                    string          `json:"key"`
	Environment            string          `json:"environment"`
	TenantID               *string         `json:"tenant_id,omitempty"`
	NewValue               json.RawMessage `json:"new_value"`
	Reason                 string          `json:"reason"`
	IncidentID             string          `json:"incident_id"`
	ActorPrincipalID       string          `json:"actor_principal_id"`
	ExpiresAt              time.Time       `json:"expires_at"`
	ActivatedConfigEntryID *string         `json:"activated_config_entry_id,omitempty"`
	PriorConfigEntryID     *string         `json:"prior_config_entry_id,omitempty"`
	RevertedToPrior        bool            `json:"reverted_to_prior"`
	Status                 string          `json:"status"`
	RetrospectiveDueAt     *time.Time      `json:"retrospective_due_at,omitempty"`
	RetrospectiveClosedAt  *time.Time      `json:"retrospective_closed_at,omitempty"`
	CreatedAt              time.Time       `json:"created_at"`
}

// CreateEmergencyChangeParams carries a POST /v1/emergency-changes write.
// ExpiresAt is required — the handler refuses a zero value with
// ErrEmergencyChangeNoExpiry.
type CreateEmergencyChangeParams struct {
	Key              string
	Environment      string
	TenantID         *string
	NewValue         json.RawMessage
	Reason           string
	IncidentID       string
	ActorPrincipalID string
	ExpiresAt        time.Time
	CallerTenantID   string
	CorrelationID    string
}

// ── Runtime attestation & drift (TC-07/08, Table 22) ─────────────────────────

// Drift classes (drift_events CHECK, Table 22).
const (
	DriftStale        = "STALE"
	DriftUnauthorized = "UNAUTHORIZED"
	DriftPartial      = "PARTIAL"
	DriftIncompatible = "INCOMPATIBLE"
	DriftUnknown      = "UNKNOWN"
)

// Drift severities.
const (
	SeverityLow      = "LOW"
	SeverityMedium   = "MEDIUM"
	SeverityHigh     = "HIGH"
	SeverityCritical = "CRITICAL"
)

// Drift remediation statuses.
const (
	RemediationOpen       = "OPEN"
	RemediationRemediated = "REMEDIATED"
	RemediationEscalated  = "ESCALATED"
)

// RuntimeAttestation is a workload's claim about which snapshot it serves
// (TC-07). The UNIQUE (runtime_id, attest_key) constraint makes the claim
// single-use: a captured attestation cannot be replayed (NP-26).
type RuntimeAttestation struct {
	AttestationID      string          `json:"attestation_id"`
	RuntimeID          string          `json:"runtime_id"`
	AttestKey          string          `json:"attest_key"`
	Environment        string          `json:"environment"`
	TenantID           *string         `json:"tenant_id,omitempty"`
	ObservedSnapshotID *string         `json:"observed_snapshot_id,omitempty"`
	ObservedEpoch      int64           `json:"observed_epoch"`
	ObservedDigest     string          `json:"observed_digest"`
	ObservedVersions   json.RawMessage `json:"observed_versions,omitempty"`
	ReportedAt         time.Time       `json:"reported_at"`
	FreshnessDeadline  time.Time       `json:"freshness_deadline"`
}

// RecordAttestationParams carries a POST /v1/runtime/attest write.
type RecordAttestationParams struct {
	RuntimeID          string
	AttestKey          string
	Environment        string
	TenantID           *string
	ObservedSnapshotID *string
	ObservedEpoch      int64
	ObservedDigest     string
	ObservedVersions   json.RawMessage
	CallerTenantID     string
}

// DriftEvent is a recorded desired/observed pair (TC-08, NP-51: exact hashes
// side by side, never compared by label).
type DriftEvent struct {
	DriftID            string     `json:"drift_id"`
	RuntimeID          string     `json:"runtime_id"`
	Environment        string     `json:"environment"`
	TenantID           *string    `json:"tenant_id,omitempty"`
	DesiredSnapshotID  *string    `json:"desired_snapshot_id,omitempty"`
	DesiredEpoch       *int64     `json:"desired_epoch,omitempty"`
	DesiredDigest      *string    `json:"desired_digest,omitempty"`
	ObservedSnapshotID *string    `json:"observed_snapshot_id,omitempty"`
	ObservedEpoch      *int64     `json:"observed_epoch,omitempty"`
	ObservedDigest     *string    `json:"observed_digest,omitempty"`
	DriftClass         string     `json:"drift_class"`
	Severity           string     `json:"severity"`
	DetectedAt         time.Time  `json:"detected_at"`
	RemediatedAt       *time.Time `json:"remediated_at,omitempty"`
	RemediationStatus  string     `json:"remediation_status"`
}

// AttestationResult is a runtime's response to POST /v1/runtime/attest: the
// recorded attestation, plus the drift finding if observed differed from
// desired (TC-08).
type AttestationResult struct {
	Attestation RuntimeAttestation `json:"attestation"`
	Drift       *DriftEvent        `json:"drift,omitempty"`
}

// ── Release plans (INV-04/07/19, Table 16/17) ─────────────────────────────────

// Release strategies (release_plans CHECK). ALL_OR_NOTHING is the only legal
// strategy for regulated/rights-affecting outcomes (INV-19: no randomized
// experimentation there).
const (
	StrategyPercentage          = "PERCENTAGE"
	StrategyProgressiveSchedule = "PROGRESSIVE_SCHEDULE"
	StrategyAllOrNothing        = "ALL_OR_NOTHING"
)

// ReleasePlan is one immutable version of a flag's rollout/targeting plan.
// Eligibility filters evaluate before any percentage (§6.2); bucketing is
// deterministic for a stable subject key + salt, so the same subject does not
// oscillate variants (INV-07); targeting rules are immutable once published
// (INV-04).
type ReleasePlan struct {
	ReleasePlanID          string          `json:"release_plan_id"`
	FlagKey                string          `json:"flag_key"`
	Environment            string          `json:"environment"`
	TenantID               *string         `json:"tenant_id,omitempty"`
	Strategy               string          `json:"strategy"`
	Salt                   string          `json:"salt"`
	BucketCount            int             `json:"bucket_count"`
	TargetingHash          string          `json:"targeting_hash"`
	TargetingRules         json.RawMessage `json:"targeting_rules"`
	Version                int             `json:"version"`
	PublishedAt            time.Time       `json:"published_at"`
	PublishedByPrincipalID string          `json:"published_by_principal_id"`
}

// CreateReleasePlanParams carries a POST /v1/flags/{key}/release-plans write.
// TargetingHash is computed by the store (md5 of the canonical targeting
// rules); Salt and BucketCount default when empty.
type CreateReleasePlanParams struct {
	FlagKey          string
	Environment      string
	TenantID         *string
	Strategy         string
	Salt             string
	BucketCount      int
	TargetingRules   json.RawMessage
	CallerTenantID   string
	ActorPrincipalID string
	CorrelationID    string
}

// FlagRetirement is a tombstone for a retired flag key (Table 17, INV-21/
// NP-20). REMOVED is not the same as RETIRED: `reusable` stays false until the
// verified consumer scan clears, so a retired key cannot be reused for a
// different semantic.
type FlagRetirement struct {
	RetirementID         string          `json:"retirement_id"`
	Key                  string          `json:"key"`
	Environment          string          `json:"environment"`
	TenantID             *string         `json:"tenant_id,omitempty"`
	FinalEnabled         bool            `json:"final_enabled"`
	FinalRollout         int             `json:"final_rollout"`
	Reusable             bool            `json:"reusable"`
	ConsumerScanEvidence json.RawMessage `json:"consumer_scan_evidence,omitempty"`
	RetiredByPrincipalID string          `json:"retired_by_principal_id"`
	RetiredAt            time.Time       `json:"retired_at"`
	RemovedAt            *time.Time      `json:"removed_at,omitempty"`
}

// RetireFlagParams carries a flag retirement write.
type RetireFlagParams struct {
	Key                  string
	Environment          string
	TenantID             *string
	FinalEnabled         bool
	FinalRollout         int
	ConsumerScanEvidence json.RawMessage
	CallerTenantID       string
	ActorPrincipalID     string
	CorrelationID        string
}

// ── Resolution & evaluation ──────────────────────────────────────────────────

// ResolveParams is the scope (and optional key filter) for POST
// /v1/config/resolve. Keys empty means every key applicable to the scope.
type ResolveParams struct {
	Environment   string
	TenantID      *string
	Keys          []string
	CorrelationID string
}

// EvaluateFlagParams pins one flag evaluation: the exact scope, a stable
// SubjectKey for deterministic bucketing, and the context attributes the
// eligibility filters read (region, plan, entitlements, ... — §6.2).
type EvaluateFlagParams struct {
	Key           string
	Environment   string
	TenantID      *string
	SubjectKey    string
	Context       map[string]any
	CorrelationID string
}

// FlagEvaluation is the snapshot-pinned outcome for one flag evaluation
// (INV-12): the enabled decision, the rollout applied, the reason, and the
// exact snapshot content that produced it — a past evaluation is reproducible
// from this struct alone.
type FlagEvaluation struct {
	Key               string               `json:"key"`
	Environment       string               `json:"environment"`
	TenantID          *string              `json:"tenant_id,omitempty"`
	FlagID            string               `json:"flag_id"`
	Enabled           bool                 `json:"enabled"`
	RolloutPercentage int                  `json:"rollout_percentage"`
	Outcome           string               `json:"outcome"`
	Reason            string               `json:"reason"`
	SnapshotID        string               `json:"snapshot_id"`
	Epoch             int64                `json:"epoch"`
	Digest            string               `json:"digest"`
	ReleasePlan       *ReleasePlanManifest `json:"release_plan,omitempty"`
	KillSwitch        *KillSwitchManifest  `json:"kill_switch,omitempty"`
	Bucket            *int                 `json:"bucket,omitempty"`
	Variant           string               `json:"variant,omitempty"`
}

// ActivateOverrideParams carries PUT /v1/config/overrides/{scope}: setting a
// value at one of the five precedence layers of INV-07. Layer names the layer;
// ScopeID is the layer's instance (tenant id, org unit id, or subject id) and
// is ignored for ScopeEnvironment. The definition's allowed_scopes gates which
// layers may override at all (INV-08).
type ActivateOverrideParams struct {
	Key              string          `json:"key"`
	Layer            string          `json:"layer"`
	Environment      string          `json:"environment"`
	ScopeID          *string         `json:"scope_id,omitempty"`
	Value            json.RawMessage `json:"value"`
	CallerTenantID   string          `json:"caller_tenant_id"`
	ActorPrincipalID string          `json:"actor_principal_id"`
	CorrelationID    string          `json:"correlation_id"`
}
