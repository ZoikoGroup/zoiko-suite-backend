// Package domain defines canonical types for schema-registry-svc.
//
// Scope (chunk 1, per docs/architecture/04-data-model.md §2.12 and
// 03-microservices.md §19): this registry stores the PAYLOAD shape of each
// event type only — the shared envelope fields (event_type, emitted_at,
// schema_version, source_service) are common across every publisher already
// and are not schema-registry-managed. Compatibility analysis is top-level
// only (properties + required); nested object/array evolution is not
// analyzed — a documented v1 limit, not an oversight.
//
// Mutation-rights gating (05-security.md §14.6 "event-contract mutation
// rights") is deferred to chunk 2, which wires this service to
// authorization-svc the same way tenant-entity-registry-svc already does.
package domain

import (
	"encoding/json"
	"errors"
	"time"
)

// EventSchema is one immutable, versioned payload schema for an event type.
// Versions are never edited or deleted — evolution always creates a new row.
type EventSchema struct {
	EventName  string          `json:"event_name"`
	Version    int             `json:"version"`
	JSONSchema json.RawMessage `json:"json_schema"`

	// CompatibilityMode is the evolution discipline this contract is held to
	// (04-data-model.md §17.2: "compatibility mode must be declared"). It is
	// recorded per version so the registry shows which discipline was in force
	// when each version was accepted, rather than only the current setting.
	CompatibilityMode string `json:"compatibility_mode"`

	// OwningService is the service that produces this event (§17.1). The first
	// question asked when a contract breaks is who owns it.
	OwningService string `json:"owning_service,omitempty"`

	RegisteredBy string    `json:"registered_by,omitempty"`
	RegisteredAt time.Time `json:"registered_at"`
}

// Compatibility modes. VARCHAR in the database and extensible by data
// migration; these two are the ones this service's checker understands.
const (
	// CompatibilityBackward requires a new version not to break existing
	// consumers of the current latest version. The default, and what the
	// service applied unconditionally before the mode was declarable.
	CompatibilityBackward = "BACKWARD"

	// CompatibilityNone skips the check entirely. Exists for the "breaking
	// changes require controlled rollout" case in §17.2 — the exemption is
	// recorded on the row so it is visible in the register rather than
	// inferred from a schema that inexplicably changed shape.
	CompatibilityNone = "NONE"
)

// ValidCompatibilityMode reports whether mode is one this service can enforce.
// An unrecognised mode is refused rather than silently treated as BACKWARD:
// accepting it would record a discipline the service is not actually applying.
func ValidCompatibilityMode(mode string) bool {
	return mode == CompatibilityBackward || mode == CompatibilityNone
}

// RegisterSchemaRequest is the wire request for POST /v1/schemas/{eventName}/versions.
type RegisterSchemaRequest struct {
	JSONSchema json.RawMessage `json:"json_schema"`

	// CompatibilityMode is optional; omitted means BACKWARD, which is both the
	// safe default and the behaviour every previously-registered schema was
	// held to, so existing callers keep working unchanged.
	CompatibilityMode string `json:"compatibility_mode,omitempty"`

	OwningService string `json:"owning_service,omitempty"`
}

var (
	// ErrEventNameRequired is returned when the event name path segment is empty.
	ErrEventNameRequired = errors.New("event name is required")
	// ErrSchemaRequired is returned when the request body has no json_schema.
	ErrSchemaRequired = errors.New("json_schema is required")
	// ErrSchemaMalformed is returned when json_schema isn't a valid JSON object.
	ErrSchemaMalformed = errors.New("json_schema must be a valid JSON object")
	// ErrEventNotFound is returned when no version exists for an event name.
	ErrEventNotFound = errors.New("event schema not found")
	// ErrVersionNotFound is returned when a specific version doesn't exist.
	ErrVersionNotFound = errors.New("event schema version not found")
	// ErrIncompatibleSchema is returned when a new version would break
	// existing consumers of the current latest version. Carries the specific
	// violations via IncompatibleSchemaError.
	ErrIncompatibleSchema = errors.New("incompatible schema change")
	// ErrStoreUnavailable is returned when Postgres is unreachable — fail closed.
	ErrStoreUnavailable = errors.New("schema store unavailable")
	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through the gateway's identity verification. Fail closed.
	ErrIdentityMissing = errors.New("caller identity missing")
	// ErrPublishDenied is returned when authorization-svc denies the
	// principal the SCHEMA_PUBLISH action.
	ErrPublishDenied = errors.New("not authorized to publish schemas")
	// ErrAuthorizationServiceUnavailable is returned when authorization-svc
	// cannot be reached — mutations fail closed, never silently permitted.
	ErrAuthorizationServiceUnavailable = errors.New("authorization service unavailable")
	// ErrInvalidCompatibilityMode is returned when the declared mode is not one
	// this service enforces. Refused rather than defaulted, so the registry
	// never records a discipline it is not applying.
	ErrInvalidCompatibilityMode = errors.New("compatibility_mode must be BACKWARD or NONE")
	// ErrVersionRaced is returned when a concurrent registration claimed the
	// version this request computed. The caller should re-read the latest
	// version and retry — the proposed schema was validated against a baseline
	// that is no longer current, so silently retrying server-side would skip
	// the compatibility check against the version that actually won.
	ErrVersionRaced = errors.New("a concurrent registration claimed this version — re-read the latest version and retry")
)

// IncompatibleSchemaError carries the specific compatibility violations found,
// so callers get an actionable 409 instead of a bare error string.
type IncompatibleSchemaError struct {
	Violations []string
}

func (e *IncompatibleSchemaError) Error() string {
	return ErrIncompatibleSchema.Error()
}

func (e *IncompatibleSchemaError) Unwrap() error {
	return ErrIncompatibleSchema
}
