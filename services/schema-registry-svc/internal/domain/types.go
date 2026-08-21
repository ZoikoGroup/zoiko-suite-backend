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
	"regexp"
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

// MaxEventNameLen and MaxOwningServiceLen mirror the VARCHAR(255) columns.
//
// Neither was checked. An over-long value reached Postgres, died there as
// SQLSTATE 22001 (string_data_right_truncation), and came back as 503 "schema
// store unavailable" — an outage status for a name that was simply too long,
// and one that sends a reader to look at the database.
const (
	MaxEventNameLen     = 255
	MaxOwningServiceLen = 255
)

// eventNameRE is the naming convention every publisher on this platform
// already follows: dotted, lowercase, at least two segments —
// `jurisdiction.rule.updated`, `entity.status.changed`.
//
// The service used to accept ANY non-empty string, and the console carried
// this same regex client-side with a comment saying so. That is the wrong
// place for it: the console is one caller of a registry whose entire purpose
// is being canonical, and a name that matches nothing anyone publishes is a
// contract for an event that does not exist. Worse, the path segment is
// echoed back in responses and stored as the key, so accepting arbitrary
// bytes made the registry's primary key a free-text field.
var eventNameRE = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z0-9]+)+$`)

// ValidEventName reports whether name is a well-formed event name.
func ValidEventName(name string) bool {
	return len(name) <= MaxEventNameLen && eventNameRE.MatchString(name)
}

// ValidateJSONSchema checks that raw is something this registry can actually
// hold as a contract, and returns nil when it is.
//
// The service used to check only `json.Valid`, whose contract is "is this
// well-formed JSON" — so `123`, `"a string"`, `null` and `[]` all passed, and
// were stored as event contracts, under an error message that claimed
// json_schema "must be a valid JSON object".
//
// That was not merely untidy. A first version stored as `123` can never be
// evolved: the next registration runs the compatibility check, compat.Check
// fails to parse the stored baseline into a shape, and every future version of
// that event answers 400 forever. The registry accepted a value that
// permanently bricked the contract it was recording.
func ValidateJSONSchema(raw json.RawMessage) error {
	if len(raw) == 0 {
		return ErrSchemaRequired
	}
	if !json.Valid(raw) {
		return ErrSchemaMalformed
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		// Well-formed JSON, but not an object.
		return ErrSchemaMalformed
	}
	if len(probe) == 0 {
		// `{}` constrains nothing. A contract that permits every payload is
		// not a contract, and recording one would let a producer claim its
		// events are governed when nothing about them is specified.
		return ErrSchemaEmpty
	}
	// `properties` and `required` are the two members the compatibility
	// checker reads. If either is present but the wrong shape, every future
	// version of this event would fail its compatibility check — so the
	// registration that introduces it is refused now, while there is still a
	// caller to tell.
	if _, err := ShapeOf(raw); err != nil {
		return ErrSchemaShapeInvalid
	}
	return nil
}

// Shape is the part of a JSON Schema this registry understands: the top-level
// property types and the required list.
type Shape struct {
	Properties map[string]struct {
		Type string `json:"type"`
	} `json:"properties"`
	Required []string `json:"required"`
}

// ShapeOf parses the analysable part of a schema. It lives in domain rather
// than in compat so the write path can reject an unparseable shape at
// registration time, using the same parse the checker will later depend on.
func ShapeOf(raw json.RawMessage) (Shape, error) {
	var s Shape
	if err := json.Unmarshal(raw, &s); err != nil {
		return Shape{}, err
	}
	return s, nil
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
	// ErrEventNameInvalid is returned when the event name is not a dotted
	// lowercase token (jurisdiction.rule.updated), the convention every
	// publisher on this platform follows.
	ErrEventNameInvalid = errors.New("event name must be dotted lowercase, e.g. entity.status.changed, and at most 255 characters")
	// ErrSchemaRequired is returned when the request body has no json_schema.
	ErrSchemaRequired = errors.New("json_schema is required")
	// ErrSchemaMalformed is returned when json_schema isn't a valid JSON object.
	ErrSchemaMalformed = errors.New("json_schema must be a valid JSON object")
	// ErrSchemaEmpty is returned for `{}` — well-formed, an object, and
	// constraining nothing. A contract that permits every payload is not a
	// contract.
	ErrSchemaEmpty = errors.New("json_schema must declare at least one member; {} constrains nothing")
	// ErrSchemaShapeInvalid is returned when `properties` or `required` are
	// present but not the shape the compatibility checker reads. Refused at
	// registration because otherwise every FUTURE version of the event would
	// fail its compatibility check against this one.
	ErrSchemaShapeInvalid = errors.New("json_schema has a properties/required member the compatibility checker cannot read")
	// ErrOwningServiceTooLong is returned when owning_service exceeds the
	// column width, which used to reach Postgres and answer 503.
	ErrOwningServiceTooLong = errors.New("owning_service must be at most 255 characters")
	// ErrFieldTooLong is what a Postgres 22001 becomes — a caller's value was
	// wider than its column. A 400, not the 503 it used to produce.
	ErrFieldTooLong = errors.New("a submitted field is too long for its column")
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
