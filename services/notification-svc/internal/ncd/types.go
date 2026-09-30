// Package ncd is the Notification, Communication & Delivery Control plane of
// ZS-SVC-Y-001: the five canonical services NCD-01 … NCD-05, implemented inside
// notification-svc the way configuration-feature-flag-svc implemented AA-001.
//
// What lives here is the part of the plane that is pure decision: the
// vocabulary (purpose, sensitivity, urgency, evidence class, the §10.3 reason
// codes), template validation and rendering against a typed variable contract
// (§4.4), the channel decision (§5), the attempt state machine (§6.2) and
// evidence normalization (§7.1). None of it touches a database or a network,
// so every rule the specification calls non-bypassable is tested as a function
// before it is tested as a route. Persistence is internal/store; the routes
// are internal/handler; the loop that moves jobs is Worker, in this package.
package ncd

import (
	"encoding/json"
	"time"
)

// ── §2.2 purpose classes ────────────────────────────────────────────────────

type PurposeClass string

const (
	PurposeSecurityCritical   PurposeClass = "SECURITY_CRITICAL"
	PurposeRegulated          PurposeClass = "REGULATED_RIGHTS_AFFECTING"
	PurposeTransactional      PurposeClass = "TRANSACTIONAL_RELATIONSHIP"
	PurposeOperational        PurposeClass = "OPERATIONAL_WORKFLOW"
	PurposeServiceInformation PurposeClass = "SERVICE_INFORMATION"
	PurposeMarketing          PurposeClass = "MARKETING_PROMOTIONAL"
)

var purposeClasses = map[PurposeClass]bool{
	PurposeSecurityCritical: true, PurposeRegulated: true, PurposeTransactional: true,
	PurposeOperational: true, PurposeServiceInformation: true, PurposeMarketing: true,
}

func (p PurposeClass) Valid() bool { return purposeClasses[p] }

// Stream maps a purpose to the sender stream it travels on. §11.1 "domain
// separation": marketing reputation must not be able to drag security mail
// into the spam folder, so each class family has its own stream, its own
// quota and its own circuit breaker.
func (p PurposeClass) Stream() string {
	switch p {
	case PurposeSecurityCritical:
		return "CRITICAL"
	case PurposeRegulated, PurposeTransactional:
		return "TRANSACTIONAL"
	case PurposeMarketing:
		return "MARKETING"
	default:
		return "OPERATIONAL"
	}
}

// Priority orders the delivery queue (NP-53: a marketing blast must not starve
// a security alert). Higher runs first.
func (p PurposeClass) Priority() int {
	switch p {
	case PurposeSecurityCritical:
		return 100
	case PurposeRegulated:
		return 80
	case PurposeTransactional:
		return 60
	case PurposeOperational:
		return 40
	case PurposeServiceInformation:
		return 30
	default:
		return 10
	}
}

// ── §2.3 independent control dimensions ─────────────────────────────────────

// Level is one of the ordered S/U/E scales ("S0".."S3", "U0".."U3", "E0".."E4").
type Level string

// Rank returns the numeric part of a level, or -1 when it is malformed.
func (l Level) Rank() int {
	if len(l) != 2 || l[1] < '0' || l[1] > '9' {
		return -1
	}
	return int(l[1] - '0')
}

func validLevel(l Level, prefix byte, max int) bool {
	return len(l) == 2 && l[0] == prefix && l.Rank() >= 0 && l.Rank() <= max
}

func ValidSensitivity(l Level) bool   { return validLevel(l, 'S', 3) }
func ValidUrgency(l Level) bool       { return validLevel(l, 'U', 3) }
func ValidEvidenceClass(l Level) bool { return validLevel(l, 'E', 4) }

// ── channels ────────────────────────────────────────────────────────────────

const (
	ChannelEmail = "EMAIL"
	ChannelInApp = "IN_APP"
	ChannelSMS   = "SMS"
	ChannelPush  = "PUSH"
)

var channels = map[string]bool{ChannelEmail: true, ChannelInApp: true, ChannelSMS: true, ChannelPush: true}

func ValidChannel(c string) bool { return channels[c] }

// ── §10.3 stable reason codes ───────────────────────────────────────────────

type ReasonCode string

const (
	NCD001IntentNotFound            ReasonCode = "NCD-001"
	NCD002IntentNotEffective        ReasonCode = "NCD-002"
	NCD003TemplateNotPublished      ReasonCode = "NCD-003"
	NCD004TemplateVariableInvalid   ReasonCode = "NCD-004"
	NCD005LocaleNotApproved         ReasonCode = "NCD-005"
	NCD006RecipientUnresolved       ReasonCode = "NCD-006"
	NCD007EndpointUnverified        ReasonCode = "NCD-007"
	NCD008PrivacyPermissionBlocked  ReasonCode = "NCD-008"
	NCD009MarketingPermissionBlock  ReasonCode = "NCD-009"
	NCD010ChannelSuppressed         ReasonCode = "NCD-010"
	NCD011NoCompliantChannel        ReasonCode = "NCD-011"
	NCD012QuietHourDeferred         ReasonCode = "NCD-012"
	NCD013ProviderRouteUnavailable  ReasonCode = "NCD-013"
	NCD014DeliveryAttemptUnknown    ReasonCode = "NCD-014"
	NCD015DeliveryExpired           ReasonCode = "NCD-015"
	NCD016EvidenceInsufficient      ReasonCode = "NCD-016"
	NCD017AcknowledgmentInvalid     ReasonCode = "NCD-017"
	NCD018RegulatedReviewRequired   ReasonCode = "NCD-018"
	NCD019DuplicateCommunication    ReasonCode = "NCD-019"
	NCD020CrossTenantRecipientBlock ReasonCode = "NCD-020"
)

// ReasonNames is §10.3's stable meaning for every code, served alongside the
// code so a caller never has to carry a lookup table.
var ReasonNames = map[ReasonCode]string{
	NCD001IntentNotFound:            "INTENT_NOT_FOUND",
	NCD002IntentNotEffective:        "INTENT_NOT_EFFECTIVE",
	NCD003TemplateNotPublished:      "TEMPLATE_NOT_PUBLISHED",
	NCD004TemplateVariableInvalid:   "TEMPLATE_VARIABLE_INVALID",
	NCD005LocaleNotApproved:         "LOCALE_NOT_APPROVED",
	NCD006RecipientUnresolved:       "RECIPIENT_UNRESOLVED",
	NCD007EndpointUnverified:        "ENDPOINT_UNVERIFIED",
	NCD008PrivacyPermissionBlocked:  "PRIVACY_PERMISSION_BLOCKED",
	NCD009MarketingPermissionBlock:  "MARKETING_PERMISSION_BLOCKED",
	NCD010ChannelSuppressed:         "CHANNEL_SUPPRESSED",
	NCD011NoCompliantChannel:        "NO_COMPLIANT_CHANNEL",
	NCD012QuietHourDeferred:         "QUIET_HOUR_DEFERRED",
	NCD013ProviderRouteUnavailable:  "PROVIDER_ROUTE_UNAVAILABLE",
	NCD014DeliveryAttemptUnknown:    "DELIVERY_ATTEMPT_UNKNOWN",
	NCD015DeliveryExpired:           "DELIVERY_EXPIRED",
	NCD016EvidenceInsufficient:      "EVIDENCE_INSUFFICIENT",
	NCD017AcknowledgmentInvalid:     "ACKNOWLEDGMENT_INVALID",
	NCD018RegulatedReviewRequired:   "REGULATED_NOTICE_REVIEW_REQUIRED",
	NCD019DuplicateCommunication:    "DUPLICATE_COMMUNICATION",
	NCD020CrossTenantRecipientBlock: "CROSS_TENANT_RECIPIENT_BLOCKED",
}

// Refusal is a stable, coded reason something did not happen.
type Refusal struct {
	Code   ReasonCode `json:"reason_code"`
	Name   string     `json:"reason"`
	Detail string     `json:"detail"`
}

func Refuse(code ReasonCode, detail string) *Refusal {
	return &Refusal{Code: code, Name: ReasonNames[code], Detail: detail}
}

func (r *Refusal) Error() string { return string(r.Code) + " " + r.Name + ": " + r.Detail }

// ── NCD-01 entities ─────────────────────────────────────────────────────────

// VariableSpec is one entry of an intent's typed variable contract (§4.2,
// §9.1 VariableContract).
type VariableSpec struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // string, number, date, money, url, email, enum
	Required bool   `json:"required"`
	// Sensitivity decides where the value may appear: an S2/S3 value may not
	// reach a subject, an SMS or a push preview (INV-17).
	Sensitivity Level `json:"sensitivity"`
	// SourceAuthority names who is authoritative for the value (§4.1).
	SourceAuthority string `json:"source_authority,omitempty"`
	// FallbackText is the schema-defined governed fallback for a missing
	// optional value (§4.4 "Missing variable"). A required variable has none.
	FallbackText  string   `json:"fallback_text,omitempty"`
	AllowedValues []string `json:"allowed_values,omitempty"`
	MaxLength     int      `json:"max_length,omitempty"`
}

// AttachmentSlot binds an attachment to a DRC record type (§4.1).
type AttachmentSlot struct {
	Slot           string `json:"slot"`
	DRCRecordType  string `json:"drc_record_type"`
	MaxSensitivity Level  `json:"max_sensitivity"`
	SecureLink     bool   `json:"secure_link"`
	Required       bool   `json:"required"`
}

// Intent is one version of a communication intent (§4.2).
type Intent struct {
	IntentID                  string           `json:"intent_id"`
	Version                   int              `json:"version"`
	TenantID                  string           `json:"tenant_id"`
	LegalEntityID             string           `json:"legal_entity_id"`
	IntentCode                string           `json:"intent_code"`
	DisplayName               string           `json:"display_name"`
	PurposeClass              PurposeClass     `json:"purpose_class"`
	DomainOwner               string           `json:"domain_owner"`
	Sensitivity               Level            `json:"sensitivity"`
	Urgency                   Level            `json:"urgency"`
	EvidenceClass             Level            `json:"evidence_class"`
	AllowedChannels           []string         `json:"allowed_channels"`
	FallbackAllowed           bool             `json:"fallback_allowed"`
	MarketingAllowed          bool             `json:"marketing_allowed"`
	Mandatory                 bool             `json:"mandatory"`
	PreferenceOverrideAllowed bool             `json:"preference_override_allowed"`
	QuietHoursPolicy          string           `json:"quiet_hours_policy"`
	BulkAllowed               bool             `json:"bulk_allowed"`
	RecordRequirement         bool             `json:"record_requirement"`
	AckRequirement            string           `json:"ack_requirement"`
	VariableContract          []VariableSpec   `json:"variable_contract"`
	AttachmentContract        []AttachmentSlot `json:"attachment_contract"`
	ApprovedURLDomains        []string         `json:"approved_url_domains"`
	DefaultExpirySeconds      int              `json:"default_expiry_seconds"`
	Status                    string           `json:"status"`
	CreatedByPrincipalID      string           `json:"created_by_principal_id"`
	CreatedAt                 time.Time        `json:"created_at"`
	ApprovedByPrincipalID     string           `json:"approved_by_principal_id,omitempty"`
	ActivatedAt               *time.Time       `json:"activated_at,omitempty"`
	EffectiveFrom             *time.Time       `json:"effective_from,omitempty"`
	RetiredAt                 *time.Time       `json:"retired_at,omitempty"`
	RetiredByPrincipalID      string           `json:"retired_by_principal_id,omitempty"`
}

// Variable returns the contract entry for name.
func (i Intent) Variable(name string) (VariableSpec, bool) {
	for _, v := range i.VariableContract {
		if v.Name == name {
			return v, true
		}
	}
	return VariableSpec{}, false
}

// Allows reports whether channel is in the intent's policy-compatible set.
func (i Intent) Allows(channel string) bool {
	for _, c := range i.AllowedChannels {
		if c == channel {
			return true
		}
	}
	return false
}

// ClassifiedAsMarketing is §11.4's anti-circumvention rule: a message is
// marketing if its intent is marketing OR its intent permits promotional
// content. Either way it must pass marketing permission.
func (i Intent) ClassifiedAsMarketing() bool {
	return i.PurposeClass == PurposeMarketing || i.MarketingAllowed
}

// Template statuses (§4.3).
const (
	TemplateDraft     = "DRAFT"
	TemplateReview    = "REVIEW"
	TemplateApproved  = "APPROVED"
	TemplatePublished = "PUBLISHED"
	TemplateRetired   = "RETIRED"
)

// TemplateVersion is one immutable channel/locale variant of an intent's
// content (§9.1 TemplateVersion).
type TemplateVersion struct {
	TemplateVersionID      string            `json:"template_version_id"`
	TemplateID             string            `json:"template_id"`
	Version                int               `json:"version"`
	TenantID               string            `json:"tenant_id"`
	LegalEntityID          string            `json:"legal_entity_id"`
	IntentID               string            `json:"intent_id"`
	IntentVersion          int               `json:"intent_version"`
	Channel                string            `json:"channel"`
	Locale                 string            `json:"locale"`
	CompatibleLocales      []string          `json:"compatible_locales"`
	Subject                string            `json:"subject"`
	Body                   string            `json:"body"`
	ContentHash            string            `json:"content_hash"`
	SchemaHash             string            `json:"schema_hash"`
	Status                 string            `json:"status"`
	CreatedByPrincipalID   string            `json:"created_by_principal_id"`
	CreatedAt              time.Time         `json:"created_at"`
	ValidatedAt            *time.Time        `json:"validated_at,omitempty"`
	ValidationReport       *ValidationReport `json:"validation_report,omitempty"`
	ApprovedByPrincipalID  string            `json:"approved_by_principal_id,omitempty"`
	ApprovedAt             *time.Time        `json:"approved_at,omitempty"`
	PublishedByPrincipalID string            `json:"published_by_principal_id,omitempty"`
	PublishedAt            *time.Time        `json:"published_at,omitempty"`
	EffectiveFrom          *time.Time        `json:"effective_from,omitempty"`
	RetiredAt              *time.Time        `json:"retired_at,omitempty"`
}

// EffectiveSet is GET /v1/intents/{id}/effective (§4.5): the exact intent
// version and template set in force at a transaction time, as known at a
// knowledge time.
type EffectiveSet struct {
	IntentID        string            `json:"intent_id"`
	TransactionTime time.Time         `json:"transaction_time"`
	KnowledgeTime   time.Time         `json:"knowledge_time"`
	Intent          Intent            `json:"intent"`
	Templates       []TemplateVersion `json:"templates"`
}

// ── NCD-02 entities ─────────────────────────────────────────────────────────

// Endpoint provenance values (§5.2).
const (
	ProvenanceIdentityContext = "IDENTITY_CONTEXT" // IAM-linked verified contact
	ProvenancePlatformInbox   = "PLATFORM_INBOX"   // the authenticated in-app inbox
	ProvenanceControlledInput = "CONTROLLED_EXCEPTION"
	ProvenanceRequest         = "REQUEST" // caller-supplied, non-regulated only
)

// Endpoint is one resolved contact route with its provenance.
type Endpoint struct {
	Channel        string    `json:"channel"`
	Address        string    `json:"-"`
	EndpointHash   string    `json:"endpoint_hash"`
	EndpointMasked string    `json:"endpoint_masked"`
	Provenance     string    `json:"provenance"`
	ProvenanceRef  string    `json:"provenance_ref,omitempty"`
	Verified       bool      `json:"verified"`
	ResolvedAt     time.Time `json:"resolved_at"`
}

// endpointJSON carries the address in storage; the public JSON of Endpoint
// never does (the address is PII and a plan is readable by operators).
type endpointJSON struct {
	Channel        string    `json:"channel"`
	Address        string    `json:"address"`
	EndpointHash   string    `json:"endpoint_hash"`
	EndpointMasked string    `json:"endpoint_masked"`
	Provenance     string    `json:"provenance"`
	ProvenanceRef  string    `json:"provenance_ref,omitempty"`
	Verified       bool      `json:"verified"`
	ResolvedAt     time.Time `json:"resolved_at"`
}

// MarshalEndpointsForStorage keeps the address; the API encoding drops it.
func MarshalEndpointsForStorage(eps []Endpoint) ([]byte, error) {
	out := make([]endpointJSON, len(eps))
	for i, e := range eps {
		out[i] = endpointJSON(e)
	}
	return json.Marshal(out)
}

func UnmarshalEndpointsFromStorage(raw []byte) ([]Endpoint, error) {
	var in []endpointJSON
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]Endpoint, len(in))
	for i, e := range in {
		out[i] = Endpoint(e)
	}
	return out, nil
}

// RecipientPlan is POST /v1/recipient-resolution's output (§5.5).
type RecipientPlan struct {
	PlanID               string         `json:"recipient_plan_id"`
	TenantID             string         `json:"tenant_id"`
	LegalEntityID        string         `json:"legal_entity_id"`
	IntentID             string         `json:"intent_id"`
	IntentVersion        int            `json:"intent_version"`
	RecipientPrincipalID string         `json:"recipient_principal_id"`
	Endpoints            []Endpoint     `json:"endpoints"`
	Locale               string         `json:"locale,omitempty"`
	TimeZone             string         `json:"time_zone,omitempty"`
	TimeZoneSource       string         `json:"time_zone_source,omitempty"`
	DecisionEvidence     map[string]any `json:"decision_evidence"`
	CreatedByPrincipalID string         `json:"created_by_principal_id"`
	CreatedAt            time.Time      `json:"created_at"`
}

// Preference is a user's convenience profile (§5.3) — not consent.
type Preference struct {
	TenantID        string    `json:"tenant_id"`
	PrincipalID     string    `json:"principal_id"`
	MutedChannels   []string  `json:"muted_channels"`
	ChannelOrder    []string  `json:"channel_order"`
	QuietHoursStart string    `json:"quiet_hours_start,omitempty"` // "HH:MM"
	QuietHoursEnd   string    `json:"quiet_hours_end,omitempty"`
	TimeZone        string    `json:"time_zone,omitempty"`
	Locale          string    `json:"locale,omitempty"`
	Version         int       `json:"version"`
	UpdatedAt       time.Time `json:"updated_at"`
	UpdatedBy       string    `json:"updated_by_principal_id"`
}

// Suppression reasons (§5.4) and sources.
const (
	SuppHardBounce       = "HARD_BOUNCE"
	SuppComplaint        = "COMPLAINT_ABUSE"
	SuppMarketingOptOut  = "MARKETING_OPTOUT"
	SuppChannelMute      = "CHANNEL_MUTE"
	SuppSecurityHold     = "SECURITY_HOLD"
	SuppLegalRestriction = "LEGAL_RESTRICTION"
	SuppSoftBounce       = "TEMP_SOFT_BOUNCE"
	SuppEndpointInvalid  = "ENDPOINT_INVALID"
)

var suppressionReasons = map[string]bool{
	SuppHardBounce: true, SuppComplaint: true, SuppMarketingOptOut: true, SuppChannelMute: true,
	SuppSecurityHold: true, SuppLegalRestriction: true, SuppSoftBounce: true, SuppEndpointInvalid: true,
}

func ValidSuppressionReason(r string) bool { return suppressionReasons[r] }

// GovernedReactivation lists the reasons whose lift needs a second principal
// and evidence (§7.3: "operator toggles alone are insufficient").
func GovernedReactivation(reason string) bool {
	switch reason {
	case SuppHardBounce, SuppComplaint, SuppLegalRestriction, SuppSecurityHold:
		return true
	}
	return false
}

// Suppression is one canonical suppression fact.
type Suppression struct {
	SuppressionID      string     `json:"suppression_id"`
	TenantID           string     `json:"tenant_id"`
	SubjectPrincipalID string     `json:"subject_principal_id,omitempty"`
	EndpointHash       string     `json:"endpoint_hash,omitempty"`
	EndpointMasked     string     `json:"endpoint_masked,omitempty"`
	ChannelScope       string     `json:"channel_scope"`
	PurposeScope       string     `json:"purpose_scope"`
	Reason             string     `json:"reason"`
	Source             string     `json:"source"`
	SourceEvidenceRef  string     `json:"source_evidence_ref"`
	EffectiveFrom      time.Time  `json:"effective_from"`
	EffectiveUntil     *time.Time `json:"effective_until,omitempty"`
	CreatedByPrincipal string     `json:"created_by_principal_id"`
	CreatedAt          time.Time  `json:"created_at"`
	LiftedAt           *time.Time `json:"lifted_at,omitempty"`
	LiftedByPrincipal  string     `json:"lifted_by_principal_id,omitempty"`
	LiftEvidenceRef    string     `json:"lift_evidence_ref,omitempty"`
	LiftApprovedBy     string     `json:"lift_approved_by_principal_id,omitempty"`
	// Legacy marks a row read from the pre-NCD email_suppressions list, which
	// the webhook processor and RFC 8058 unsubscribe still write. Read so that
	// no suppression recorded before this plane existed is forgotten (NP-45).
	Legacy bool `json:"legacy,omitempty"`
}

// ActiveAt reports whether the suppression is in force at t.
func (s Suppression) ActiveAt(t time.Time) bool {
	if s.LiftedAt != nil && !s.LiftedAt.After(t) {
		return false
	}
	if s.EffectiveFrom.After(t) {
		return false
	}
	if s.EffectiveUntil != nil && !s.EffectiveUntil.After(t) {
		return false
	}
	return true
}

// Covers reports whether the suppression applies to a channel and purpose.
func (s Suppression) Covers(channel string, purpose PurposeClass) bool {
	if s.ChannelScope != "ALL" && s.ChannelScope != channel {
		return false
	}
	if s.PurposeScope != "ALL" && s.PurposeScope != string(purpose) {
		return false
	}
	return true
}

// PermissionDecision is a referenced PRV (or PRV+PDC marketing) decision
// (§5.3). NCD consumes it; it never computes one.
type PermissionDecision struct {
	Decision   string `json:"decision"` // PERMIT, RESTRICT, DENY, INDETERMINATE
	DecisionID string `json:"decision_id,omitempty"`
	// Restrictions a RESTRICT decision attaches, e.g. ["NO_SMS"].
	Restrictions []string `json:"restrictions,omitempty"`
}

// ── NCD-03 entities ─────────────────────────────────────────────────────────

// Binding is a certified provider route (§6.3).
type Binding struct {
	BindingID          string   `json:"binding_id"`
	Channel            string   `json:"channel"`
	ProviderName       string   `json:"provider_name"`
	Regions            []string `json:"regions"`
	EvidenceCapability Level    `json:"evidence_capability"`
	SupportsReceipts   bool     `json:"supports_receipts"`
	SenderIdentity     string   `json:"sender_identity"`
	Certified          bool     `json:"certified"`
	FailoverGroup      string   `json:"failover_group"`
	Priority           int      `json:"priority"`
	CostRank           int      `json:"cost_rank"`
	CallbackSecretEnv  string   `json:"callback_secret_env,omitempty"`
	Status             string   `json:"status"`
	Health             string   `json:"health"`
	HealthReason       string   `json:"health_reason,omitempty"`
}

// Route is one ordered entry of a channel plan.
type Route struct {
	Channel            string `json:"channel"`
	BindingID          string `json:"binding_id"`
	EvidenceCapability Level  `json:"evidence_capability"`
	FailoverGroup      string `json:"failover_group"`
	EndpointHash       string `json:"endpoint_hash"`
	EndpointMasked     string `json:"endpoint_masked"`
	Provenance         string `json:"provenance"`
	// Address travels in storage only (see routeJSON); never in API output.
	Address string `json:"-"`
}

type routeJSON struct {
	Channel            string `json:"channel"`
	BindingID          string `json:"binding_id"`
	EvidenceCapability Level  `json:"evidence_capability"`
	FailoverGroup      string `json:"failover_group"`
	EndpointHash       string `json:"endpoint_hash"`
	EndpointMasked     string `json:"endpoint_masked"`
	Provenance         string `json:"provenance"`
	Address            string `json:"address"`
}

func MarshalRoutesForStorage(rs []Route) ([]byte, error) {
	out := make([]routeJSON, len(rs))
	for i, r := range rs {
		out[i] = routeJSON(r)
	}
	return json.Marshal(out)
}

func UnmarshalRoutesFromStorage(raw []byte) ([]Route, error) {
	var in []routeJSON
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]Route, len(in))
	for i, r := range in {
		out[i] = Route(r)
	}
	return out, nil
}

// Restriction is a route the decision excluded, and why.
type Restriction struct {
	Channel      string     `json:"channel"`
	EndpointHash string     `json:"endpoint_hash,omitempty"`
	Code         ReasonCode `json:"reason_code"`
	Detail       string     `json:"detail"`
}

// Decision outcomes (§5.5).
const (
	DecisionPermitted      = "PERMITTED"
	DecisionDeferred       = "DEFERRED"
	DecisionBlocked        = "BLOCKED"
	DecisionReviewRequired = "REVIEW_REQUIRED"
)

// ChannelDecision is POST /v1/channel-decision's output (§5.5).
type ChannelDecision struct {
	DecisionID          string         `json:"decision_id"`
	TenantID            string         `json:"tenant_id"`
	PlanID              string         `json:"recipient_plan_id"`
	IntentID            string         `json:"intent_id"`
	IntentVersion       int            `json:"intent_version"`
	CommunicationID     string         `json:"communication_id,omitempty"`
	Outcome             string         `json:"outcome"`
	Routes              []Route        `json:"routes"`
	Restrictions        []Restriction  `json:"restrictions"`
	ReasonCodes         []ReasonCode   `json:"reason_codes"`
	NotBefore           *time.Time     `json:"not_before,omitempty"`
	EvidenceRequirement Level          `json:"evidence_requirement"`
	FallbackRules       map[string]any `json:"fallback_rules"`
	Inputs              map[string]any `json:"inputs"`
	DecidedBy           string         `json:"decided_by_principal_id"`
	DecidedAt           time.Time      `json:"decided_at"`
}

// Communication lifecycle states.
const (
	CommCreated        = "CREATED"
	CommPrepared       = "PREPARED"
	CommBlocked        = "BLOCKED"
	CommReviewRequired = "REVIEW_REQUIRED"
	CommDispatched     = "DISPATCHED"
	CommCompleted      = "COMPLETED"
	CommException      = "EXCEPTION"
	CommExpired        = "EXPIRED"
	CommCancelled      = "CANCELLED"
)

// Attachment is a DRC record pinned by version and hash (INV-18).
type Attachment struct {
	Slot        string `json:"slot"`
	DRCRecordID string `json:"drc_record_id"`
	DRCVersion  string `json:"drc_version"`
	SHA256      string `json:"sha256"`
}

// FreeTextEndpoint is a caller-supplied endpoint (§5.2). For a regulated
// notice it is refused unless a controlled exception names its authority,
// verification and a reviewer distinct from the caller (NP-11).
type FreeTextEndpoint struct {
	Channel             string `json:"channel"`
	Address             string `json:"address"`
	ExceptionRef        string `json:"exception_ref,omitempty"`
	VerificationRef     string `json:"verification_ref,omitempty"`
	ReviewerPrincipalID string `json:"reviewer_principal_id,omitempty"`
}

// Communication is the §10.1 logical communication.
type Communication struct {
	CommunicationID           string             `json:"communication_id"`
	TenantID                  string             `json:"tenant_id"`
	LegalEntityID             string             `json:"legal_entity_id"`
	IntentID                  string             `json:"intent_id"`
	IntentVersion             int                `json:"intent_version,omitempty"`
	PurposeClass              PurposeClass       `json:"purpose_class,omitempty"`
	IdempotencyKey            string             `json:"idempotency_key"`
	SourceEventID             string             `json:"source_event_id"`
	SourceEventType           string             `json:"source_event_type,omitempty"`
	WorkflowID                string             `json:"workflow_id,omitempty"`
	RecipientPrincipalID      string             `json:"recipient_principal_id"`
	RecipientTenantID         string             `json:"recipient_tenant_id,omitempty"`
	FreeTextEndpoint          *FreeTextEndpoint  `json:"-"`
	Locale                    string             `json:"locale"`
	Variables                 map[string]string  `json:"-"`
	Attachments               []Attachment       `json:"attachments"`
	PrivacyPermission         PermissionDecision `json:"privacy_permission"`
	MarketingPermission       PermissionDecision `json:"marketing_permission"`
	PDCDecisionRef            string             `json:"pdc_decision_ref,omitempty"`
	ResidencyRegions          []string           `json:"residency_regions"`
	LifecycleState            string             `json:"lifecycle_state"`
	BlockedReasonCode         ReasonCode         `json:"blocked_reason_code,omitempty"`
	BlockedDetail             string             `json:"blocked_detail,omitempty"`
	RecipientPlanID           string             `json:"recipient_plan_id,omitempty"`
	ChannelDecisionID         string             `json:"channel_decision_id,omitempty"`
	NotBefore                 *time.Time         `json:"not_before,omitempty"`
	ExpiresAt                 time.Time          `json:"expires_at"`
	Priority                  int                `json:"priority"`
	SupersedesCommunicationID string             `json:"supersedes_communication_id,omitempty"`
	CorrectionReason          string             `json:"correction_reason,omitempty"`
	BulkID                    string             `json:"bulk_id,omitempty"`
	CreatedByPrincipalID      string             `json:"created_by_principal_id"`
	CreatedAt                 time.Time          `json:"created_at"`
	PreparedAt                *time.Time         `json:"prepared_at,omitempty"`
	DispatchedAt              *time.Time         `json:"dispatched_at,omitempty"`
	ConcludedAt               *time.Time         `json:"concluded_at,omitempty"`
}

// RenderedContent is the exact as-issued content for one channel.
type RenderedContent struct {
	RenderID           string            `json:"render_id"`
	CommunicationID    string            `json:"communication_id"`
	Channel            string            `json:"channel"`
	TemplateVersionID  string            `json:"template_version_id"`
	TemplateVersion    int               `json:"template_version"`
	Locale             string            `json:"locale"`
	LocaleFallbackFrom string            `json:"locale_fallback_from,omitempty"`
	Subject            string            `json:"-"`
	Body               string            `json:"-"`
	SubjectHash        string            `json:"subject_hash"`
	BodyHash           string            `json:"body_hash"`
	ContentHash        string            `json:"content_hash"`
	VariableHashes     map[string]string `json:"variable_hashes"`
	AttachmentManifest []Attachment      `json:"attachment_manifest"`
	RenderedAt         time.Time         `json:"rendered_at"`
}

// Job states (§6.1).
const (
	JobQueued             = "QUEUED"
	JobAwaitingEvidence   = "AWAITING_EVIDENCE"
	JobAwaitingResolution = "AWAITING_RESOLUTION"
	JobCompleted          = "COMPLETED"
	JobException          = "EXCEPTION"
	JobExpired            = "EXPIRED"
	JobCancelled          = "CANCELLED"
)

// DeliveryJob is one channel-plan execution container (§6.1).
type DeliveryJob struct {
	JobID               string     `json:"job_id"`
	TenantID            string     `json:"tenant_id"`
	CommunicationID     string     `json:"communication_id"`
	Origin              string     `json:"origin"`
	ResendReason        string     `json:"resend_reason,omitempty"`
	Routes              []Route    `json:"routes"`
	RouteIndex          int        `json:"route_index"`
	AttemptsOnRoute     int        `json:"attempts_on_route"`
	MaxAttemptsPerRoute int        `json:"max_attempts_per_route"`
	State               string     `json:"state"`
	Stream              string     `json:"stream"`
	Priority            int        `json:"priority"`
	NextRunAt           time.Time  `json:"next_run_at"`
	NotBefore           *time.Time `json:"not_before,omitempty"`
	ExpiresAt           time.Time  `json:"expires_at"`
	LastDeferralReason  string     `json:"last_deferral_reason,omitempty"`
	ExceptionReason     string     `json:"exception_reason,omitempty"`
	CreatedBy           string     `json:"created_by_principal_id"`
	CreatedAt           time.Time  `json:"created_at"`
	ConcludedAt         *time.Time `json:"concluded_at,omitempty"`
	LeasedUntil         *time.Time `json:"-"`
}

// Attempt states (§6.2).
const (
	AttemptCreated    = "CREATED"
	AttemptSubmitting = "SUBMITTING"
	AttemptAccepted   = "ACCEPTED"
	AttemptPending    = "PENDING"
	AttemptUnknown    = "UNKNOWN"
	AttemptDelivered  = "DELIVERED"
	AttemptFailed     = "FAILED"
	AttemptBounced    = "BOUNCED"
)

// Attempt is one immutable provider submission (§6.1, §9.1 DeliveryAttempt).
type Attempt struct {
	AttemptID            string         `json:"attempt_id"`
	TenantID             string         `json:"tenant_id"`
	JobID                string         `json:"job_id"`
	CommunicationID      string         `json:"communication_id"`
	RouteIndex           int            `json:"route_index"`
	Channel              string         `json:"channel"`
	BindingID            string         `json:"binding_id"`
	IntentID             string         `json:"intent_id"`
	IntentVersion        int            `json:"intent_version"`
	PurposeClass         PurposeClass   `json:"purpose_class"`
	RecipientPrincipalID string         `json:"recipient_principal_id"`
	Origin               string         `json:"origin"`
	IdempotencyToken     string         `json:"idempotency_token"`
	ContentHash          string         `json:"content_hash"`
	RenderID             string         `json:"render_id"`
	RecipientSnapshot    map[string]any `json:"recipient_snapshot"`
	State                string         `json:"state"`
	ProviderMessageID    string         `json:"provider_message_id,omitempty"`
	FailureReason        string         `json:"failure_reason,omitempty"`
	Retryable            bool           `json:"retryable"`
	ResolutionDueAt      *time.Time     `json:"resolution_due_at,omitempty"`
	ResolvedAt           *time.Time     `json:"resolved_at,omitempty"`
	ResolvedBy           string         `json:"resolved_by_principal_id,omitempty"`
	ResolutionNote       string         `json:"resolution_note,omitempty"`
	CreatedAt            time.Time      `json:"created_at"`
	SubmittedAt          *time.Time     `json:"submitted_at,omitempty"`
	StateChangedAt       time.Time      `json:"state_changed_at"`
}

// ── NCD-04 entities ─────────────────────────────────────────────────────────

// Evidence is one normalized delivery/interaction fact (§7.1).
type Evidence struct {
	EvidenceID           string         `json:"evidence_id"`
	TenantID             string         `json:"tenant_id"`
	CommunicationID      string         `json:"communication_id"`
	AttemptID            string         `json:"attempt_id,omitempty"`
	NoticeID             string         `json:"notice_id,omitempty"`
	EvidenceType         string         `json:"evidence_type"`
	NormalizedState      string         `json:"normalized_state"`
	Source               string         `json:"source"`
	Confidence           string         `json:"confidence"`
	DoesNotProve         string         `json:"does_not_prove"`
	BindingID            string         `json:"binding_id,omitempty"`
	ProviderEventID      string         `json:"provider_event_id,omitempty"`
	PayloadHash          string         `json:"payload_hash,omitempty"`
	ObservedAt           time.Time      `json:"observed_at"`
	ReceivedAt           time.Time      `json:"received_at"`
	SupersedesEvidenceID string         `json:"supersedes_evidence_id,omitempty"`
	ActorPrincipalID     string         `json:"actor_principal_id,omitempty"`
	Details              map[string]any `json:"details,omitempty"`
}

// ── NCD-05 entities ─────────────────────────────────────────────────────────

// Notice states (§8.2).
const (
	NoticePrepared          = "PREPARED"
	NoticeReady             = "READY"
	NoticeInProgress        = "DELIVERY_IN_PROGRESS"
	NoticeDeliveryEvidenced = "DELIVERY_EVIDENCED"
	NoticeSatisfiedByPolicy = "SATISFIED_BY_POLICY"
	NoticeAckPending        = "ACK_PENDING"
	NoticeAcknowledged      = "ACKNOWLEDGED"
	NoticeDeclined          = "DECLINED"
	NoticeExpired           = "EXPIRED"
	NoticeDisputed          = "DISPUTED"
	NoticeException         = "EXCEPTION"
	NoticeSuperseded        = "SUPERSEDED"
)

// Acknowledgment requirements (§8.1).
const (
	AckNone             = "NONE"
	AckReceipt          = "RECEIPT"
	AckAuthenticatedAck = "AUTHENTICATED_ACK"
	AckAcceptance       = "ACCEPTANCE"
)

func ValidAckRequirement(a string) bool {
	switch a {
	case AckNone, AckReceipt, AckAuthenticatedAck, AckAcceptance:
		return true
	}
	return false
}

// RegulatedNotice is the §8.1 notice package.
type RegulatedNotice struct {
	NoticeID             string       `json:"notice_id"`
	NoticeVersion        int          `json:"notice_version"`
	TenantID             string       `json:"tenant_id"`
	LegalEntityID        string       `json:"legal_entity_id"`
	CommunicationID      string       `json:"communication_id"`
	LegalBasisRef        string       `json:"legal_basis_ref"`
	RecipientCapacity    string       `json:"recipient_capacity"`
	DeliveryMethods      []string     `json:"delivery_methods"`
	ContentHash          string       `json:"content_hash"`
	AttachmentManifest   []Attachment `json:"attachment_manifest"`
	Locale               string       `json:"locale"`
	EffectiveDate        string       `json:"effective_date,omitempty"`
	WFCObligationRef     string       `json:"wfc_obligation_ref,omitempty"`
	DeadlineAt           *time.Time   `json:"deadline_at,omitempty"`
	AckRequirement       string       `json:"ack_requirement"`
	EvidenceRequirement  Level        `json:"evidence_requirement"`
	RecordRequirement    bool         `json:"record_requirement"`
	RecordStatus         string       `json:"record_status"`
	DRCRecordRef         string       `json:"drc_record_ref,omitempty"`
	State                string       `json:"state"`
	AtRiskNotifiedAt     *time.Time   `json:"at_risk_notified_at,omitempty"`
	SupersedesNoticeID   string       `json:"supersedes_notice_id,omitempty"`
	SupersessionReason   string       `json:"supersession_reason,omitempty"`
	DispositionRef       string       `json:"disposition_ref,omitempty"`
	CreatedByPrincipalID string       `json:"created_by_principal_id"`
	CreatedAt            time.Time    `json:"created_at"`
	StateChangedAt       time.Time    `json:"state_changed_at"`
	// LegalSufficiency is stated on every read and never computed: §8.5 and
	// INV-22 — NCD does not decide legal sufficiency.
	LegalSufficiency string `json:"legal_sufficiency"`
}

// LegalSufficiencyNotDetermined is the only value NCD ever reports.
const LegalSufficiencyNotDetermined = "NOT_DETERMINED_BY_NCD"

// Acknowledgment is §9.1 Acknowledgment.
type Acknowledgment struct {
	AckID            string    `json:"ack_id"`
	TenantID         string    `json:"tenant_id"`
	NoticeID         string    `json:"notice_id"`
	NoticeVersion    int       `json:"notice_version"`
	CommunicationID  string    `json:"communication_id"`
	ActorPrincipalID string    `json:"actor_principal_id"`
	Method           string    `json:"method"`
	Disposition      string    `json:"disposition"`
	ContentHash      string    `json:"content_hash"`
	EvidenceRef      string    `json:"evidence_ref,omitempty"`
	Comment          string    `json:"comment,omitempty"`
	AcknowledgedAt   time.Time `json:"acknowledged_at"`
}

// Approval is one maker-checker request.
type Approval struct {
	ApprovalID    string         `json:"approval_id"`
	TenantID      string         `json:"tenant_id"`
	LegalEntityID string         `json:"legal_entity_id"`
	Kind          string         `json:"kind"`
	TargetID      string         `json:"target_id"`
	Payload       map[string]any `json:"payload"`
	Reason        string         `json:"reason"`
	Status        string         `json:"status"`
	RequestedBy   string         `json:"requested_by_principal_id"`
	RequestedAt   time.Time      `json:"requested_at"`
	DecidedBy     string         `json:"decided_by_principal_id,omitempty"`
	DecidedAt     *time.Time     `json:"decided_at,omitempty"`
	DecisionNote  string         `json:"decision_note,omitempty"`
}

// Approval kinds.
const (
	ApprovalResend          = "RESEND"
	ApprovalManualEvidence  = "MANUAL_EVIDENCE"
	ApprovalSuppressionLift = "SUPPRESSION_LIFT"
	ApprovalBulkSend        = "BULK_SEND"
)

// Exception is an item on the human path.
type Exception struct {
	ExceptionID     string     `json:"exception_id"`
	TenantID        string     `json:"tenant_id"`
	CommunicationID string     `json:"communication_id,omitempty"`
	AttemptID       string     `json:"attempt_id,omitempty"`
	NoticeID        string     `json:"notice_id,omitempty"`
	Kind            string     `json:"kind"`
	ReasonCode      ReasonCode `json:"reason_code,omitempty"`
	Detail          string     `json:"detail"`
	CreatedAt       time.Time  `json:"created_at"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy      string     `json:"resolved_by_principal_id,omitempty"`
	Resolution      string     `json:"resolution,omitempty"`
}

// BulkSend is a governed bulk expansion (§6.5).
type BulkSend struct {
	BulkID           string            `json:"bulk_id"`
	TenantID         string            `json:"tenant_id"`
	LegalEntityID    string            `json:"legal_entity_id"`
	IntentID         string            `json:"intent_id"`
	SourceEventID    string            `json:"source_event_id"`
	Recipients       []string          `json:"recipients"`
	AudienceCount    int               `json:"audience_count"`
	AudienceHash     string            `json:"audience_hash"`
	Variables        map[string]string `json:"-"`
	Locale           string            `json:"locale"`
	RequiresApproval bool              `json:"requires_approval"`
	ApprovalID       string            `json:"approval_id,omitempty"`
	Status           string            `json:"status"`
	RequestedBy      string            `json:"requested_by_principal_id"`
	CreatedAt        time.Time         `json:"created_at"`
	DispatchedAt     *time.Time        `json:"dispatched_at,omitempty"`
}
