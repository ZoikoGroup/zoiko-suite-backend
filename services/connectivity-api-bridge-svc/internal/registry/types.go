// Package registry is XIC-01, the Integration Contract & Connector Registry of ZS-SVC-T-001 (§6).
//
// A Connector is a stable identity for a class of external system. A ContractVersion is the immutable,
// reviewable statement of exactly how that class may communicate: operations, endpoint and auth profiles,
// source and canonical schemas, mapping versions, idempotency and callback semantics, limits and
// certification requirements. Nothing is ever edited after it leaves DRAFT; a correction is a new version
// (I-03). The registry owns transport and contract semantics only — domain meaning stays with the domain.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// PlatformScopeID is the platform-scope tenant. Registry content is platform reference data: tenants read what is
// published, only the platform scope writes.
const PlatformScopeID = "00000000-0000-0000-0000-00000000f001"

// Connector lifecycle (§4).
const (
	ConnectorDraft      = "DRAFT"
	ConnectorReview     = "REVIEW"
	ConnectorPublished  = "PUBLISHED"
	ConnectorDeprecated = "DEPRECATED"
	ConnectorRetired    = "RETIRED"
)

// Contract version lifecycle (§4 and Figure 4: REVIEW and BLOCKED are the review stage and its failure exit).
const (
	VersionDraft      = "DRAFT"
	VersionValidated  = "VALIDATED"
	VersionReview     = "REVIEW"
	VersionPublished  = "PUBLISHED"
	VersionSuperseded = "SUPERSEDED"
	VersionRetired    = "RETIRED"
	VersionBlocked    = "BLOCKED"
)

// Mapping version lifecycle.
const (
	MappingDraft      = "DRAFT"
	MappingPublished  = "PUBLISHED"
	MappingSuperseded = "SUPERSEDED"
	MappingRetired    = "RETIRED"
)

// Connector families (§17.2 domain adoption list).
var Families = []string{
	"BANK_FEED", "PAYMENT_RAIL", "TAX_AUTHORITY", "EINVOICE_NETWORK", "PAYROLL", "HRIS", "BENEFITS",
	"ESIGNATURE", "ERP", "CRM", "REGULATOR", "FILE_TRANSFER", "API",
}

// Directions and channels.
const (
	Inbound  = "INBOUND"
	Outbound = "OUTBOUND"
)

var inboundChannels = map[string]bool{"WEBHOOK": true, "API": true, "EVENT": true, "SFTP": true, "OBJECT": true}
var outboundChannels = map[string]bool{"HTTPS": true, "SFTP": true, "OBJECT": true}

// Profile kinds.
const (
	ProfileAuth          = "AUTH"
	ProfileEndpoint      = "ENDPOINT"
	ProfileIdempotency   = "IDEMPOTENCY"
	ProfileCallback      = "CALLBACK"
	ProfileCertification = "CERTIFICATION"
)

// Publication gates (§6.3). SCHEMA_COMPAT is evidenced by the registry's own validation; the rest are attested
// by people other than the author.
const (
	GateSchemaCompat     = "SCHEMA_COMPAT"
	GateSecurity         = "SECURITY"
	GatePrivacy          = "PRIVACY"
	GateNegativeTests    = "NEGATIVE_TESTS"
	GateDomainAcceptance = "DOMAIN_ACCEPTANCE"
	GateRollback         = "ROLLBACK"
)

// RequiredGates lists every gate a version must pass before publication.
var RequiredGates = []string{GateSchemaCompat, GateSecurity, GatePrivacy, GateNegativeTests, GateDomainAcceptance, GateRollback}

// SystemAttestor attests SCHEMA_COMPAT: the registry itself, from a validation run.
const SystemAttestor = "system:xic-registry"

// Gate outcomes.
const (
	GatePassed = "PASSED"
	GateFailed = "FAILED"
)

// Compatibility classes (§6.4), in increasing severity.
const (
	ClassNone                  = "NONE"
	ClassPotentiallyCompatible = "POTENTIALLY_COMPATIBLE"
	ClassLifecycle             = "LIFECYCLE"
	ClassMaterialSecurity      = "MATERIAL_SECURITY"
	ClassMaterialBusiness      = "MATERIAL_BUSINESS"
	ClassMaterialEgress        = "MATERIAL_EGRESS"
	ClassBreaking              = "BREAKING"
)

var classRank = map[string]int{ClassNone: 0, ClassPotentiallyCompatible: 1, ClassLifecycle: 2,
	ClassMaterialSecurity: 3, ClassMaterialBusiness: 3, ClassMaterialEgress: 3, ClassBreaking: 4}

// Errors.
var (
	ErrNotPlatformScope  = errors.New("registry content can only be written from the platform scope")
	ErrConnectorNotFound = errors.New("connector not found")
	ErrVersionNotFound   = errors.New("contract version not found")
	ErrMappingNotFound   = errors.New("mapping version not found")
	ErrConnectorExists   = errors.New("connector already exists")
	ErrVersionExists     = errors.New("contract version already exists for this connector")
	ErrVersionConflict   = errors.New("version conflict: the object changed since it was read")
	ErrIdempotency       = errors.New("idempotency key was used for a different request")
	ErrNotDraft          = errors.New("contract version is not a draft and cannot be edited; create a new version")
	ErrTransition        = errors.New("transition is not allowed from the current state")
	ErrGatesIncomplete   = errors.New("publication gates are not all passed")
	ErrSelfAttest        = errors.New("a gate must be attested by someone other than the author")
	ErrSelfPublish       = errors.New("a contract must be published by someone other than the author")
	ErrBaselineMoved     = errors.New("another version was published since validation; validate again")
	ErrNotValidated      = errors.New("contract version has not passed validation")
	ErrStaleValidation   = errors.New("contract content changed after validation; validate again")
	ErrVersionOrder      = errors.New("semantic version must be greater than every published version of this connector")
	ErrBadRequest        = errors.New("bad request")
	ErrDeprecateWindow   = errors.New("deprecation needs a supported-until date in the future")
)

// Issue is one reason a contract is invalid. It names a location and a rule, never a value from the contract.
type Issue struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

// ValidationError is a contract that cannot be accepted, with every reason found.
type ValidationError struct{ Issues []Issue }

func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString("contract is invalid:")
	for i, is := range e.Issues {
		if i == 8 {
			fmt.Fprintf(&b, " … and %d more", len(e.Issues)-8)
			break
		}
		fmt.Fprintf(&b, " %s (%s);", is.Path, is.Rule)
	}
	return b.String()
}

// Connector is the stable identity of an external system class.
type Connector struct {
	ConnectorID    string     `json:"connector_id"`
	Family         string     `json:"family"`
	ProviderClass  string     `json:"provider_class"`
	Owner          string     `json:"owner"`
	Lifecycle      string     `json:"lifecycle"`
	SupportedUntil *time.Time `json:"supported_until,omitempty"`
	Version        int64      `json:"version"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

var connectorIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{2,62}$`)

// Validate checks a connector identity.
func (c *Connector) Validate() error {
	var is []Issue
	if !connectorIDRE.MatchString(c.ConnectorID) {
		is = append(is, Issue{"connector_id", "lowercase letters, digits and hyphens, 3 to 63 characters, starting with a letter"})
	}
	if !contains(Families, c.Family) {
		is = append(is, Issue{"family", "not a supported connector family"})
	}
	if strings.TrimSpace(c.ProviderClass) == "" || len(c.ProviderClass) > 128 {
		is = append(is, Issue{"provider_class", "required, at most 128 characters"})
	}
	if strings.TrimSpace(c.Owner) == "" || len(c.Owner) > 128 {
		is = append(is, Issue{"owner", "required, at most 128 characters"})
	}
	if len(is) > 0 {
		return &ValidationError{is}
	}
	return nil
}

// Operation is one registered, callable operation. Only registered operations are callable (§6.2).
type Operation struct {
	ID                 string `json:"operation_id"`
	Direction          string `json:"direction"`
	Channel            string `json:"channel"`
	Method             string `json:"method,omitempty"`
	EndpointTemplate   string `json:"endpoint_template,omitempty"`
	AuthProfile        string `json:"auth_profile"`
	EndpointProfile    string `json:"endpoint_profile,omitempty"`
	IdempotencyProfile string `json:"idempotency_profile,omitempty"`
	CallbackProfile    string `json:"callback_profile,omitempty"`
	RequestSchema      string `json:"request_schema,omitempty"`
	ResponseSchema     string `json:"response_schema,omitempty"`
	Irreversible       bool   `json:"irreversible,omitempty"`
	// ProviderIdempotency says whether the provider honours an idempotency key on this operation, and the
	// header that carries it. OD-07: an irreversible operation cannot be published without it.
	ProviderIdempotency ProviderIdempotency `json:"provider_idempotency"`
	UnknownResolution   UnknownResolution   `json:"unknown_resolution"`

	// INBOUND only. Where in the payload the sender names the tenant and the provider account, as JSON pointers. They
	// are corroboration, never authority: a disagreement with the authenticated connection is a security event.
	PayloadTenantPointer  string `json:"payload_tenant_pointer,omitempty"`
	PayloadAccountPointer string `json:"payload_account_pointer,omitempty"`
	// SchemaVersionHeader names the header carrying the exact source schema version the sender used.
	SchemaVersionHeader string `json:"schema_version_header,omitempty"`
	// ResubmitAfterUnknown is true only where the provider's idempotency key makes a same-attempt resubmission
	// safe (NP-20). Without it, an UNKNOWN attempt is resolved, never resubmitted (NP-21).
	ResubmitAfterUnknown bool       `json:"resubmit_after_unknown,omitempty"`
	MaxPayloadBytes      int64      `json:"max_payload_bytes,omitempty"`
	RateLimitPerMinute   int        `json:"rate_limit_per_minute,omitempty"`
	DataClasses          []string   `json:"data_classes"`
	AllowedRegions       []string   `json:"allowed_regions"`
	Deprecated           bool       `json:"deprecated,omitempty"`
	SupportedUntil       *time.Time `json:"supported_until,omitempty"`
}

// ProviderIdempotency describes the provider's own key support.
type ProviderIdempotency struct {
	Supported bool   `json:"supported"`
	Header    string `json:"header,omitempty"`
}

// UnknownResolution names how an UNKNOWN outbound attempt is resolved (I-14): a status query on another
// registered operation, a callback, reconciliation, or any combination. At least one is required.
type UnknownResolution struct {
	StatusQueryOperation string `json:"status_query_operation,omitempty"`
	ViaCallback          bool   `json:"via_callback,omitempty"`
	ViaReconciliation    bool   `json:"via_reconciliation,omitempty"`
}

// Schema is one exact schema version.
type Schema struct {
	Ref               string          `json:"schema"` // id@version
	Direction         string          `json:"direction"`
	MediaType         string          `json:"media_type"`
	MaxSizeBytes      int64           `json:"max_size_bytes"`
	ValidationProfile json.RawMessage `json:"validation_profile"`
}

// Profile is a named, kinded profile. Its Spec is validated per kind.
type Profile struct {
	Kind string          `json:"kind"`
	Name string          `json:"name"`
	Spec json.RawMessage `json:"spec"`
}

// MappingDef declares a mapping version inside a contract.
type MappingDef struct {
	Key               string          `json:"mapping_key"`
	Version           int             `json:"version"`
	SourceSchema      string          `json:"source_schema"`
	CanonicalContract string          `json:"canonical_contract_ref"`
	Spec              json.RawMessage `json:"spec"`
}

// Content is everything a contract version contains.
type Content struct {
	Operations []Operation  `json:"operations"`
	Schemas    []Schema     `json:"schemas"`
	Profiles   []Profile    `json:"profiles"`
	Mappings   []MappingDef `json:"mappings"`
}

// ContractVersion is one version of a connector's contract.
type ContractVersion struct {
	ContractVersionID string     `json:"contract_version_id"`
	ConnectorID       string     `json:"connector_id"`
	Semver            string     `json:"semver"`
	Status            string     `json:"status"`
	EffectiveFrom     time.Time  `json:"effective_from"`
	SupportedUntil    *time.Time `json:"supported_until,omitempty"`

	MigrationPlanRef  string `json:"migration_plan_ref,omitempty"`
	ReplayAnalysisRef string `json:"replay_analysis_ref,omitempty"`
	RollbackRef       string `json:"rollback_ref,omitempty"`

	Content         Content          `json:"content"`
	MappingVersions []MappingVersion `json:"mapping_versions,omitempty"`
	ContentHash     string           `json:"content_hash,omitempty"`

	BaselineVersionID string        `json:"baseline_version_id,omitempty"`
	CompatClass       string        `json:"compat_class,omitempty"`
	CompatReport      *CompatReport `json:"compat_report,omitempty"`
	BlockedReason     string        `json:"blocked_reason,omitempty"`

	RowVersion  int64      `json:"version"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ValidatedAt *time.Time `json:"validated_at,omitempty"`
	PublishedBy string     `json:"published_by,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// MappingVersion is a mapping as stored: owned by its contract version, immutable once that leaves DRAFT.
type MappingVersion struct {
	MappingVersionID  string          `json:"mapping_version_id"`
	ContractVersionID string          `json:"contract_version_id"`
	Key               string          `json:"mapping_key"`
	Version           int             `json:"version"`
	SourceSchema      string          `json:"source_schema"`
	CanonicalContract string          `json:"canonical_contract_ref"`
	Spec              json.RawMessage `json:"spec"`
	TransformHash     string          `json:"transform_hash"`
	Status            string          `json:"status"`
}

// Gate is one attestation about a version. Attestations are append-only; the latest per gate counts.
type Gate struct {
	GateKey     string          `json:"gate"`
	Outcome     string          `json:"outcome"`
	EvidenceRef string          `json:"evidence_ref,omitempty"`
	Detail      json.RawMessage `json:"detail,omitempty"`
	AttestedBy  string          `json:"attested_by"`
	At          time.Time       `json:"at"`
}

// ValidationRun is the append-only record of one validation.
type ValidationRun struct {
	RunID       string        `json:"run_id"`
	Outcome     string        `json:"outcome"` // PASSED | FAILED
	ContentHash string        `json:"content_hash"`
	Issues      []Issue       `json:"issues,omitempty"`
	Report      *CompatReport `json:"compat_report,omitempty"`
	RunBy       string        `json:"run_by"`
	At          time.Time     `json:"at"`
}

// Transition is the append-only evidence of a status change.
type Transition struct {
	From   string    `json:"from"`
	To     string    `json:"to"`
	Actor  string    `json:"actor"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// Event is a domain event to be written to the outbox in the same transaction as the change it describes.
type Event struct {
	Type          string
	AggregateType string
	AggregateID   string
	ActorID       string
	CorrelationID string
	Data          map[string]any
}

// Actor is who is acting.
type Actor struct {
	PrincipalID   string
	CorrelationID string
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// EventTypes lists every event the registry emits (ZS-SVC-T-001 section 14.1, integration.contract.* and
// integration.connector.*). A test proves each one is produced by a real flow and is a valid envelope.
var EventTypes = []string{
	"integration.connector.created", "integration.connector.deprecated",
	"integration.contract.draft-created", "integration.contract.draft-updated",
	"integration.contract.validated", "integration.contract.validation-failed",
	"integration.contract.submitted", "integration.contract.gate-attested", "integration.contract.blocked",
	"integration.contract.published", "integration.contract.superseded",
}
