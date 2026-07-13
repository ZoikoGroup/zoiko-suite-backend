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
	EventName    string          `json:"event_name"`
	Version      int             `json:"version"`
	JSONSchema   json.RawMessage `json:"json_schema"`
	RegisteredBy string          `json:"registered_by,omitempty"`
	RegisteredAt time.Time       `json:"registered_at"`
}

// RegisterSchemaRequest is the wire request for POST /v1/schemas/{eventName}/versions.
type RegisterSchemaRequest struct {
	JSONSchema json.RawMessage `json:"json_schema"`
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
