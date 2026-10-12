// Package envelope builds the ZoikoSuite canonical event envelope defined by
// ZS-EVENT-001 §4: a CloudEvents 1.0-aligned envelope plus ZoikoSuite
// business-context extension attributes.
//
// WHY A SHARED PACKAGE. Before this existed every producer hand-wrote its own
// envelope struct, and 24 of them drifted apart: event id in the body in some,
// only in a Kafka header in others; occurred_at here, emitted_at there; no
// payload hash or aggregate version anywhere. A consumer written against one
// producer silently mis-read another. New refuses to build an envelope that is
// missing a required attribute, so drift becomes a compile-or-test failure in
// the producer instead of a data problem in some consumer.
//
// EXPAND / CONTRACT. Every consumer in the estate today parses the legacy
// field names (event_id, event_type, emitted_at, tenant_id, ...). ZS-EVENT-001
// §6.1 requires producers to "remain backward compatible with active consumers
// during contract migration", so by default the envelope carries BOTH the
// canonical attributes and the legacy names. Once every consumer reads the
// canonical names, producers switch legacy off (Spec.OmitLegacyFields) and the
// legacy block is deleted from this package.
package envelope

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SpecVersion is the CloudEvents specification family (ZS-EVENT-001 §4:
// "CloudEvents core specification 1.0.2 ... its specversion value for the 1.0
// family remains 1.0").
const SpecVersion = "1.0"

// DataContentType is the only payload media type this package produces.
const DataContentType = "application/json"

// Classification values (ZS-EVENT-001 §4: PUBLIC / INTERNAL / CONFIDENTIAL /
// RESTRICTED). Serialized lower-case as in the standard's canonical example.
type Classification string

const (
	Public       Classification = "public"
	Internal     Classification = "internal"
	Confidential Classification = "confidential"
	Restricted   Classification = "restricted"
)

func (c Classification) valid() bool {
	switch c {
	case Public, Internal, Confidential, Restricted:
		return true
	}
	return false
}

var (
	// com.zoikosuite.<domain>.<aggregate>.<past-tense-fact> (§5.1). Lower-case
	// kebab segments; at least three after the prefix.
	typePattern = regexp.MustCompile(`^com\.zoikosuite\.[a-z0-9-]+\.[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	// "Semantic event type; no version suffix" (§4) — reject a trailing .v1.
	versionSuffix = regexp.MustCompile(`\.v[0-9]+$`)
	semver        = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	// Lower-case kebab identifiers for service names, aggregate types and
	// region codes, so they are safe inside a URN.
	token = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// Spec is what a producer supplies. Everything the standard marks required is
// either a required field here or derived by New.
type Spec struct {
	// Type is the canonical semantic type, e.g.
	// "com.zoikosuite.accounting.journal.posted".
	Type string
	// LegacyType is the pre-standard event_type existing consumers filter on,
	// e.g. "journal.posted". Only emitted while legacy fields are on. Defaults
	// to Type when empty.
	LegacyType string

	// Service is the producing service, e.g. "general-ledger-svc". Becomes
	// source = urn:zoikosuite:service:<Service> and the legacy source_service.
	Service string

	// SchemaVersion is the payload contract version, SemVer (§4).
	SchemaVersion string
	// DataSchema overrides the derived registered-schema URI. Normally empty:
	// urn:zoikosuite:schema:<type without com.zoikosuite.>:<SchemaVersion>,
	// the form the standard's canonical example uses.
	DataSchema string

	// OccurredAt is when the fact occurred / was committed (§4 "time").
	OccurredAt time.Time
	// EffectiveAt is the business-effective time where distinct from
	// OccurredAt (§4 "effectiveat", conditional).
	EffectiveAt *time.Time

	TenantID      string // required (§4)
	LegalEntityID string // required for entity-bound business facts
	BookID        string // required for book-specific accounting facts

	// AggregateType and AggregateID identify the owned aggregate the fact is
	// about. AggregateType is a lower-case kebab noun ("journal") and builds
	// subject = urn:zoikosuite:<AggregateType>:<AggregateID>.
	AggregateType    string
	AggregateID      string
	AggregateVersion *int64 // post-commit version, for ordering/gap detection

	CorrelationID string
	CausationID   string
	RequestID     string

	// ActorID and Jurisdiction are not ZS-EVENT-001 §4 attributes, but Doc 03
	// §19 requires every event to carry "actor ID or system principal" and
	// "jurisdiction context". Carried as optional extension attributes so a
	// producer can meet both documents.
	ActorID      string
	Jurisdiction string

	// ResidencyRegion is the authoritative regional data-bearing context (§4,
	// required), a controlled lower-case code such as "uk" or "eu-west".
	ResidencyRegion string
	Classification  Classification

	TraceParent string // W3C trace context, optional

	// Data is the event-specific semantic fact payload. Marshalled once by
	// New; payloadhash is computed over exactly those bytes.
	Data any

	// OmitLegacyFields drops the pre-standard field names. Leave false until
	// every consumer of the producer's topic reads the canonical names.
	OmitLegacyFields bool
}

// Envelope is a validated, immutable event ready for the outbox.
type Envelope struct {
	ID               string
	Type             string
	Source           string
	Subject          string
	Time             time.Time
	DataSchema       string
	TenantID         string
	LegalEntityID    string
	BookID           string
	AggregateType    string
	AggregateID      string
	AggregateVersion *int64
	CorrelationID    string
	CausationID      string
	RequestID        string
	EffectiveAt      *time.Time
	ActorID          string
	Jurisdiction     string
	ResidencyRegion  string
	Classification   Classification
	SchemaVersion    string
	PayloadHash      string
	TraceParent      string
	Data             json.RawMessage

	service    string
	legacyType string
	legacy     bool
}

// ErrInvalid wraps every validation failure from New, so a caller can tell
// "my event is malformed" (a bug to fix) from an infrastructure error.
var ErrInvalid = errors.New("envelope: invalid event")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// New validates spec and builds the envelope, allocating its immutable id.
//
// The id is allocated HERE, before the producer's transaction commits
// (ZS-EVENT-001 §6.1 "Allocate event_id before transaction commit"), and the
// outbox never replaces it — a publish retry re-sends the same id, which is
// what lets every consumer's dedupe recognise the retry as the same fact.
func New(s Spec) (*Envelope, error) {
	if !typePattern.MatchString(s.Type) {
		return nil, invalid("type %q must match com.zoikosuite.<domain>.<aggregate>.<fact>", s.Type)
	}
	if versionSuffix.MatchString(s.Type) {
		return nil, invalid("type %q must not carry a version suffix; versions belong in schemaversion", s.Type)
	}
	if !token.MatchString(s.Service) {
		return nil, invalid("service %q must be a lower-case kebab identifier", s.Service)
	}
	if !semver.MatchString(s.SchemaVersion) {
		return nil, invalid("schema version %q must be SemVer MAJOR.MINOR.PATCH", s.SchemaVersion)
	}
	if s.OccurredAt.IsZero() {
		return nil, invalid("occurred-at time is required")
	}
	if strings.TrimSpace(s.TenantID) == "" {
		return nil, invalid("tenant id is required")
	}
	if !token.MatchString(s.ResidencyRegion) {
		return nil, invalid("residency region %q must be a controlled lower-case code", s.ResidencyRegion)
	}
	if !s.Classification.valid() {
		return nil, invalid("classification %q must be public, internal, confidential or restricted", s.Classification)
	}
	if (s.AggregateType == "") != (s.AggregateID == "") {
		return nil, invalid("aggregate type and aggregate id must be given together")
	}
	if s.AggregateType != "" && !token.MatchString(s.AggregateType) {
		return nil, invalid("aggregate type %q must be a lower-case kebab noun", s.AggregateType)
	}
	if s.AggregateVersion != nil && s.AggregateID == "" {
		return nil, invalid("aggregate version given without an aggregate")
	}
	if s.AggregateVersion != nil && *s.AggregateVersion < 0 {
		return nil, invalid("aggregate version must not be negative")
	}
	if s.Data == nil {
		return nil, invalid("data is required")
	}

	data, err := json.Marshal(s.Data)
	if err != nil {
		return nil, invalid("data does not marshal to JSON: %v", err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, invalid("data must be a JSON object")
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("envelope: allocate event id: %w", err)
	}

	dataSchema := s.DataSchema
	if dataSchema == "" {
		dataSchema = "urn:zoikosuite:schema:" + strings.TrimPrefix(s.Type, "com.zoikosuite.") + ":" + s.SchemaVersion
	}
	subject := ""
	if s.AggregateID != "" {
		subject = "urn:zoikosuite:" + s.AggregateType + ":" + s.AggregateID
	}
	legacyType := s.LegacyType
	if legacyType == "" {
		legacyType = s.Type
	}

	return &Envelope{
		ID:               id.String(),
		Type:             s.Type,
		Source:           "urn:zoikosuite:service:" + s.Service,
		Subject:          subject,
		Time:             s.OccurredAt.UTC(),
		DataSchema:       dataSchema,
		TenantID:         s.TenantID,
		LegalEntityID:    s.LegalEntityID,
		BookID:           s.BookID,
		AggregateType:    s.AggregateType,
		AggregateID:      s.AggregateID,
		AggregateVersion: s.AggregateVersion,
		CorrelationID:    s.CorrelationID,
		CausationID:      s.CausationID,
		RequestID:        s.RequestID,
		EffectiveAt:      s.EffectiveAt,
		ActorID:          s.ActorID,
		Jurisdiction:     s.Jurisdiction,
		ResidencyRegion:  s.ResidencyRegion,
		Classification:   s.Classification,
		SchemaVersion:    s.SchemaVersion,
		PayloadHash:      PayloadHash(data),
		TraceParent:      s.TraceParent,
		Data:             data,
		service:          s.Service,
		legacyType:       legacyType,
		legacy:           !s.OmitLegacyFields,
	}, nil
}

// PayloadHash is the integrity reference for the canonical serialized data
// (§4 "payloadhash"): "sha256:" + lower-case hex over the exact bytes of the
// envelope's data member. A consumer verifies it by hashing the raw `data`
// bytes as received — which is why the outbox stores the rendered envelope as
// bytes, not JSONB (JSONB re-orders keys and would change the bytes).
func PayloadHash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PartitionKey is the Kafka message key: an opaque hash of tenant and
// aggregate (§5.3 "an opaque partition key derived from tenant_id and
// aggregate_id, for example a stable cryptographic hash"). Every event for one
// aggregate lands on one partition, and no business-readable value ever
// reaches broker metadata. An event with no aggregate keys on tenant alone.
func (e *Envelope) PartitionKey() string {
	sum := sha256.Sum256([]byte(e.TenantID + "\x00" + e.AggregateType + "\x00" + e.AggregateID))
	return hex.EncodeToString(sum[:])
}

// wire is the JSON shape. publishedat is NOT here: it is the time the
// dispatcher first published the committed record (§4), unknown when the
// producer builds the envelope, and stamped in by WithPublishedAt.
type wire struct {
	SpecVersion      string     `json:"specversion"`
	ID               string     `json:"id"`
	Source           string     `json:"source"`
	Type             string     `json:"type"`
	Subject          string     `json:"subject,omitempty"`
	Time             time.Time  `json:"time"`
	DataContentType  string     `json:"datacontenttype"`
	DataSchema       string     `json:"dataschema"`
	TenantID         string     `json:"tenantid"`
	LegalEntityID    string     `json:"legalentityid,omitempty"`
	BookID           string     `json:"bookid,omitempty"`
	AggregateID      string     `json:"aggregateid,omitempty"`
	AggregateVersion *int64     `json:"aggregateversion,omitempty"`
	CorrelationID    string     `json:"correlationid,omitempty"`
	CausationID      string     `json:"causationid,omitempty"`
	RequestID        string     `json:"requestid,omitempty"`
	EffectiveAt      *time.Time `json:"effectiveat,omitempty"`
	ActorID          string     `json:"actorid,omitempty"`
	Jurisdiction     string     `json:"jurisdiction,omitempty"`
	ResidencyRegion  string     `json:"residencyregion"`
	Classification   string     `json:"classification"`
	SchemaVersionCE  string     `json:"schemaversion"`
	PayloadHash      string     `json:"payloadhash"`
	TraceParent      string     `json:"traceparent,omitempty"`

	// Legacy names, present only while Spec.OmitLegacyFields is false. These
	// are the names the estate's consumers parse today (audit-event-store,
	// workflow-history, search-indexer, financial-close, bank-reconciliation,
	// identity-context, authorization).
	LegacyEventID       string `json:"event_id,omitempty"`
	LegacyEventType     string `json:"event_type,omitempty"`
	LegacyEmittedAt     string `json:"emitted_at,omitempty"`
	LegacySchemaVersion string `json:"schema_version,omitempty"`
	LegacySourceService string `json:"source_service,omitempty"`
	LegacyTenantID      string `json:"tenant_id,omitempty"`
	LegacyLegalEntityID string `json:"legal_entity_id,omitempty"`
	LegacyActorID       string `json:"actor_id,omitempty"`
	LegacyCorrelationID string `json:"correlation_id,omitempty"`

	Data json.RawMessage `json:"data"`
	// LegacyPayload duplicates data under the old member name. The bytes are
	// identical, so payloadhash verifies against either.
	LegacyPayload json.RawMessage `json:"payload,omitempty"`
}

// MarshalJSON renders the envelope as it will be written to the topic.
func (e *Envelope) MarshalJSON() ([]byte, error) {
	w := wire{
		SpecVersion:      SpecVersion,
		ID:               e.ID,
		Source:           e.Source,
		Type:             e.Type,
		Subject:          e.Subject,
		Time:             e.Time,
		DataContentType:  DataContentType,
		DataSchema:       e.DataSchema,
		TenantID:         e.TenantID,
		LegalEntityID:    e.LegalEntityID,
		BookID:           e.BookID,
		AggregateID:      e.AggregateID,
		AggregateVersion: e.AggregateVersion,
		CorrelationID:    e.CorrelationID,
		CausationID:      e.CausationID,
		RequestID:        e.RequestID,
		EffectiveAt:      e.EffectiveAt,
		ActorID:          e.ActorID,
		Jurisdiction:     e.Jurisdiction,
		ResidencyRegion:  e.ResidencyRegion,
		Classification:   string(e.Classification),
		SchemaVersionCE:  e.SchemaVersion,
		PayloadHash:      e.PayloadHash,
		TraceParent:      e.TraceParent,
		Data:             e.Data,
	}
	if e.legacy {
		w.LegacyEventID = e.ID
		w.LegacyEventType = e.legacyType
		w.LegacyEmittedAt = e.Time.Format(time.RFC3339Nano)
		w.LegacySchemaVersion = e.SchemaVersion
		w.LegacySourceService = e.service
		w.LegacyTenantID = e.TenantID
		w.LegacyLegalEntityID = e.LegalEntityID
		w.LegacyActorID = e.ActorID
		w.LegacyCorrelationID = e.CorrelationID
		w.LegacyPayload = e.Data
	}
	return json.Marshal(w)
}

// WithPublishedAt stamps publishedat into a rendered envelope.
//
// It inserts the member textually right after the opening brace instead of
// decoding and re-encoding, so every other byte — in particular the data
// member that payloadhash covers — is delivered exactly as the producer
// committed it.
func WithPublishedAt(rendered []byte, publishedAt time.Time) ([]byte, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(rendered, &probe); err != nil {
		return nil, fmt.Errorf("envelope: rendered event is not a JSON object: %w", err)
	}
	if _, exists := probe["publishedat"]; exists {
		return nil, errors.New("envelope: rendered event already carries publishedat")
	}
	trimmed := bytes.TrimLeft(rendered, " \t\r\n")
	ts, err := json.Marshal(publishedAt.UTC())
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(trimmed)+len(ts)+16)
	out = append(out, '{')
	out = append(out, `"publishedat":`...)
	out = append(out, ts...)
	rest := bytes.TrimLeft(trimmed[1:], " \t\r\n")
	if len(rest) > 0 && rest[0] != '}' {
		out = append(out, ',')
	}
	out = append(out, rest...)
	return out, nil
}
