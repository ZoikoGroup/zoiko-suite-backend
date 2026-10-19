package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ── Evidence records (append-only in the database) ───────────────────────────

// Change is one configuration change: which protected fields moved, and which
// scopes were added or removed. Field names only, never values.
type Change struct {
	ChangeID         string    `json:"change_id"`
	ConnectorID      string    `json:"connector_id"`
	TenantID         string    `json:"tenant_id"`
	FromVersion      int64     `json:"from_version"`
	ToVersion        int64     `json:"to_version"`
	Kind             string    `json:"change_kind"`
	ChangedBy        string    `json:"changed_by"`
	ChangedFields    []string  `json:"changed_fields"`
	ScopesAdded      []string  `json:"scopes_added"`
	ScopesRemoved    []string  `json:"scopes_removed"`
	PriorFingerprint string    `json:"prior_fingerprint,omitempty"`
	NewFingerprint   string    `json:"new_fingerprint,omitempty"`
	Reason           string    `json:"reason,omitempty"`
	ChangedAt        time.Time `json:"changed_at"`
}

// Change kinds.
const (
	ChangeCreate   = "CREATE"
	ChangeUpdate   = "UPDATE"
	ChangeSubmit   = "SUBMIT"
	ChangeApprove  = "APPROVE"
	ChangeReject   = "REJECT"
	ChangeActivate = "ACTIVATE"
	ChangeSuspend  = "SUSPEND"
	ChangeRevoke   = "REVOKE"
	ChangeArchive  = "ARCHIVE"
	ChangeDegrade  = "DEGRADE"
	ChangeRecover  = "RECOVER"
	ChangeConsent  = "CONSENT"
	ChangeProfile  = "PROFILE_UPGRADE"
)

// Approval is a decision bound to the exact configuration it covered.
type Approval struct {
	ApprovalID       string    `json:"approval_id"`
	ConnectorID      string    `json:"connector_id"`
	TenantID         string    `json:"tenant_id"`
	ConnectorVersion int64     `json:"connector_version"`
	Fingerprint      string    `json:"fingerprint"`
	Decision         string    `json:"decision"`
	RequestedBy      string    `json:"requested_by"`
	DecidedBy        string    `json:"decided_by"`
	Reason           string    `json:"reason,omitempty"`
	DecidedAt        time.Time `json:"decided_at"`
}

// Approval decisions.
const (
	DecisionApproved = "APPROVED"
	DecisionRejected = "REJECTED"
)

// ── Health ────────────────────────────────────────────────────────────────────

// Health states.
const (
	HealthHealthy   = "HEALTHY"
	HealthDegraded  = "DEGRADED"
	HealthUnhealthy = "UNHEALTHY"
)

// Health reason codes. Stable: alerts and runbooks key on them.
const (
	ReasonCredentialExpired  = "CREDENTIAL_EXPIRED"
	ReasonCredentialRevoked  = "CREDENTIAL_REVOKED"
	ReasonConsentExpiring    = "CONSENT_EXPIRING"
	ReasonProfileDeprecated  = "PROFILE_DEPRECATED"
	ReasonProfileUnsupported = "PROFILE_UNSUPPORTED"
	ReasonProviderOutage     = "PROVIDER_OUTAGE"
	ReasonIngestionFailures  = "INGESTION_FAILURES"
	ReasonResidencyConflict  = "RESIDENCY_CONFLICT"
	ReasonOperatorReported   = "OPERATOR_REPORTED"
	ReasonRecovered          = "RECOVERED"
)

// Health is a connector's current health (ConnectorHealth, §9).
type Health struct {
	ConnectorID         string     `json:"connector_id"`
	TenantID            string     `json:"tenant_id"`
	State               string     `json:"state"`
	ReasonCode          string     `json:"reason_code,omitempty"`
	Detail              string     `json:"detail,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastCheckedAt       time.Time  `json:"last_checked_at"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// HealthTransition is one change of health state.
type HealthTransition struct {
	TransitionID string    `json:"transition_id"`
	ConnectorID  string    `json:"connector_id"`
	TenantID     string    `json:"tenant_id"`
	FromState    string    `json:"from_state"`
	ToState      string    `json:"to_state"`
	ReasonCode   string    `json:"reason_code,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	At           time.Time `json:"at"`
}

// HealthUpdate is the health state to write. The caller (which has read the
// current Health) decides the new state and failure count; the store records it
// and, when the state changed, an append-only transition.
type HealthUpdate struct {
	State               string
	ReasonCode          string
	Detail              string
	ConsecutiveFailures int
	Success             bool // also stamps last_success_at
}

// FailureThreshold is how many consecutive ingestion failures degrade a
// connector.
const FailureThreshold = 5

// ── Sync checkpoints ──────────────────────────────────────────────────────────

// Checkpoint is an incremental synchronisation cursor (SyncCheckpoint, §9).
type Checkpoint struct {
	ConnectorID string     `json:"connector_id"`
	TenantID    string     `json:"tenant_id"`
	Stream      string     `json:"stream"`
	Cursor      string     `json:"cursor"`
	Watermark   *time.Time `json:"watermark,omitempty"`
	Version     int64      `json:"checkpoint_version"`
	UpdatedBy   string     `json:"updated_by"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ResetRequest is a request to move a checkpoint backwards. It needs a second
// person's decision before it takes effect.
type ResetRequest struct {
	RequestID   string     `json:"request_id"`
	ConnectorID string     `json:"connector_id"`
	TenantID    string     `json:"tenant_id"`
	Stream      string     `json:"stream"`
	ToCursor    string     `json:"to_cursor"`
	ToWatermark *time.Time `json:"to_watermark,omitempty"`
	Reason      string     `json:"reason"`
	RequestedBy string     `json:"requested_by"`
	Status      string     `json:"status"`
	DecidedBy   string     `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Reset request states.
const (
	ResetPending  = "PENDING"
	ResetApplied  = "APPLIED"
	ResetRejected = "REJECTED"
)

var (
	ErrCheckpointConflict = errors.New("the checkpoint was advanced by someone else; re-read and retry")
	ErrCheckpointRegress  = errors.New("a checkpoint can only move forward; use an approved reset to move it back")
	ErrCheckpointNotFound = errors.New("no such checkpoint")
	ErrResetNotFound      = errors.New("no such checkpoint reset request")
	ErrResetNotPending    = errors.New("that reset request has already been decided")
	ErrProfileNotFound    = errors.New("no such provider profile")
	ErrProfileExists      = errors.New("that provider profile version already exists")
	// ErrPlatformScopeRequired: provider profiles are platform reference data and
	// can be written only from the platform scope.
	ErrPlatformScopeRequired = errors.New("provider profiles can only be published from the platform scope")
)

var streamPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidStream reports whether s is a legal checkpoint stream name.
func ValidStream(s string) bool { return streamPattern.MatchString(s) }

// MaxCursorLen bounds a checkpoint cursor.
const MaxCursorLen = 2048

// ── Provider profiles ─────────────────────────────────────────────────────────

// Profile statuses move forward only: DRAFT, CERTIFIED, DEPRECATED, UNSUPPORTED.
const (
	ProfileDraft       = "DRAFT"
	ProfileCertified   = "CERTIFIED"
	ProfileDeprecated  = "DEPRECATED"
	ProfileUnsupported = "UNSUPPORTED"
)

// Provider types.
var providerTypes = map[string]bool{
	"BANK": true, "PSP": true, "TAX_AUTHORITY": true, "EINVOICE_NETWORK": true, "IDP": true,
	"HRIS": true, "ESIGNATURE": true, "FILE_TRANSFER": true, "API": true,
}

// Profile is an ExternalProfile (§9): one version of a provider's certified
// integration profile. A connector pins one version; it is never silently moved
// to another (INT-CTRL-025).
type Profile struct {
	ProfileID           string     `json:"profile_id"`
	ProviderKey         string     `json:"provider_key"`
	ProviderType        string     `json:"provider_type"`
	Version             int        `json:"version"`
	Status              string     `json:"status"`
	AllowedRegions      []string   `json:"allowed_regions"`
	AllowedHostSuffixes []string   `json:"allowed_host_suffixes"`
	SupportedScopes     []string   `json:"supported_scopes"`
	SchemaRef           string     `json:"schema_ref,omitempty"`
	CertificationRef    string     `json:"certification_ref,omitempty"`
	PublishedBy         string     `json:"published_by"`
	CreatedAt           time.Time  `json:"created_at"`
	DeprecatedAt        *time.Time `json:"deprecated_at,omitempty"`
	SunsetAt            *time.Time `json:"sunset_at,omitempty"`
}

// Validate checks a profile before it is published.
func (p *Profile) Validate() error {
	var bad []string
	if !tokenPattern.MatchString(p.ProviderKey) {
		bad = append(bad, "provider_key must be a lower-case identifier")
	}
	if !providerTypes[p.ProviderType] {
		bad = append(bad, "provider_type is not a known provider type")
	}
	if p.Version < 1 {
		bad = append(bad, "version must be 1 or greater")
	}
	if len(p.AllowedRegions) == 0 {
		bad = append(bad, "at least one allowed region is required")
	}
	for _, r := range p.AllowedRegions {
		if !tokenPattern.MatchString(r) {
			bad = append(bad, "every allowed region must be a region code")
			break
		}
	}
	if len(p.AllowedHostSuffixes) == 0 {
		bad = append(bad, "at least one allowed host suffix is required: an empty allowlist would allow nothing to be verified")
	}
	for _, h := range p.AllowedHostSuffixes {
		h = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(h)), ".")
		if !strings.Contains(h, ".") || strings.ContainsAny(h, "/:@ *") {
			bad = append(bad, "every host suffix must be a registrable domain such as bank-x.example")
			break
		}
	}
	if len(p.SupportedScopes) == 0 {
		bad = append(bad, "at least one supported scope is required")
	}
	for _, s := range p.SupportedScopes {
		if !scopePattern.MatchString(s) {
			bad = append(bad, "every supported scope must be a lower-case identifier")
			break
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: %s", ErrIncompleteConfig, strings.Join(bad, "; "))
	}
	return nil
}

// AllowsRegion reports whether the profile permits a destination region.
func (p *Profile) AllowsRegion(region string) bool {
	for _, r := range p.AllowedRegions {
		if strings.EqualFold(r, region) {
			return true
		}
	}
	return false
}

// SupportsScopes returns the scopes the profile does not support.
func (p *Profile) UnsupportedScopes(scopes []string) []string {
	have := make(map[string]struct{}, len(p.SupportedScopes))
	for _, s := range p.SupportedScopes {
		have[strings.ToLower(s)] = struct{}{}
	}
	var missing []string
	for _, s := range scopes {
		if _, ok := have[strings.ToLower(s)]; !ok {
			missing = append(missing, s)
		}
	}
	return missing
}

// ── Effects: what a store transaction writes beside the connector row ─────────

// OutboxEvent is a domain event to be enqueued in the same transaction as the
// state change it describes. Type is the fact ("connector.activated"); the
// store makes it com.zoikosuite.connectivity.<Type>. LegacyType, when set, is
// the pre-standard name existing consumers filter on.
type OutboxEvent struct {
	Type             string
	LegacyType       string
	AggregateType    string // defaults to "connector"
	AggregateID      string
	AggregateVersion *int64
	LegalEntityID    string
	ActorID          string
	CorrelationID    string
	Data             map[string]any
}

// Effects are the rows and events a connector mutation writes in the same
// transaction as the connector update itself.
type Effects struct {
	Change    *Change
	Approval  *Approval
	Health    *HealthUpdate
	Ingestion *IngestionLog
	Events    []OutboxEvent
}

// MutateFunc is a state change applied to a locked connector row. It receives
// the connector and a copy of its current health (HEALTHY when none has been
// recorded), and may mutate the connector in place; returning an error discards
// everything.
type MutateFunc func(b *ApiBridge, h *Health) (*Effects, error)
