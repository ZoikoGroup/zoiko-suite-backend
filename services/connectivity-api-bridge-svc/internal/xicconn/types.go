// Package xicconn is XIC-02, the Connection Lifecycle & Credential Binding Service of ZS-SVC-T-001 (§7).
//
// A Connection binds one tenant and environment to an approved connector, pinned to a PUBLISHED contract version of
// the registry (XIC-01). Credentials are bound as REFERENCES into the vault and never read: the connection stores
// the reference, a scope fingerprint, expiry and rotation metadata, and nothing else about a secret. The connection
// is the single place an exchange asks "may this tenant use this operation of this connector right now", and
// Operable answers it with the XIC error codes of the specification.
package xicconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
)

// Connection states (§4).
const (
	StatusDraft       = "DRAFT"
	StatusAuthorizing = "AUTHORIZING"
	StatusValidating  = "VALIDATING"
	StatusActive      = "ACTIVE"
	StatusDegraded    = "DEGRADED"
	StatusSuspended   = "SUSPENDED"
	StatusRevoked     = "REVOKED"
)

// Environments.
const (
	EnvProduction = "PRODUCTION"
	EnvStaging    = "STAGING"
	EnvSandbox    = "SANDBOX"
)

// Binding states.
const (
	BindingOK       = "OK"
	BindingExpiring = "EXPIRING"
	BindingExpired  = "EXPIRED"
	BindingRevoked  = "REVOKED"
)

// Check kinds and outcomes.
const (
	CheckValidation = "VALIDATION"
	CheckProbe      = "PROBE"
	Passed          = "PASSED"
	Failed          = "FAILED"
)

// Request kinds and states.
const (
	RequestMigration  = "MIGRATION"
	RequestBreakGlass = "BREAK_GLASS"
	ReqPending        = "PENDING"
	ReqApproved       = "APPROVED"
	ReqRejected       = "REJECTED"
	ReqExpired        = "EXPIRED"
	ReqUsed           = "USED"
)

// Certification states, computed from the latest certification and what it covers (I-29).
const (
	CertMissing = "MISSING"
	CertValid   = "VALID"
	CertExpired = "EXPIRED"
	CertStale   = "STALE"
	CertFailed  = "FAILED"
	// CertNotRequired is a sandbox or staging connection: certification is a production gate.
	CertNotRequired = "NOT_REQUIRED"
)

// Errors.
var (
	ErrNotFound              = errors.New("connection not found")
	ErrExists                = errors.New("a live connection already exists for this tenant, environment, connector and provider account")
	ErrVersionConflict       = errors.New("version conflict: the connection changed since it was read")
	ErrIdempotency           = errors.New("idempotency key was used for a different request")
	ErrTransition            = errors.New("transition is not allowed from the current state")
	ErrBadRequest            = errors.New("bad request")
	ErrSelfAction            = errors.New("that step must be taken by someone other than the person who prepared it")
	ErrNotValidated          = errors.New("the connection has no passed validation of its current configuration")
	ErrNotCertified          = errors.New("a production connection needs a valid certification for this contract")
	ErrScopesInsufficient    = errors.New("the granted scopes do not cover the scopes the contract requires")
	ErrExcessScopes          = errors.New("the granted scopes exceed what the contract requires and no review acknowledgement was given")
	ErrCredentialInvalid     = errors.New("the credential reference could not be confirmed")
	ErrCredentialExpired     = errors.New("the credential has expired")
	ErrCrossTenantCredential = errors.New("the credential reference is already bound elsewhere")
	ErrDependency            = errors.New("a dependency needed to check this change is unavailable")
	ErrAuthState             = errors.New("authorization state is invalid, expired or already used")
	ErrRedirectNotRegistered = errors.New("redirect target is not pre-registered")
	ErrContractUnavailable   = errors.New("the contract version is not available for this action")
	ErrRegionDenied          = errors.New("the region is not permitted for this connection")
	ErrEgressDenied          = errors.New("the destination is not an allowed egress target")
	ErrRequestNotFound       = errors.New("request not found")
	ErrRequestState          = errors.New("request is not in a state that allows this")
	ErrBreakGlassDenied      = errors.New("break-glass cannot do that")
	ErrRotation              = errors.New("rotation is not allowed")
)

// Validation is a request that failed shape checks, with every reason.
type Validation struct{ Issues []registry.Issue }

func (v *Validation) Error() string {
	parts := make([]string, 0, len(v.Issues))
	for i, is := range v.Issues {
		if i == 6 {
			break
		}
		parts = append(parts, is.Path+" ("+is.Rule+")")
	}
	return "request is invalid: " + strings.Join(parts, "; ")
}

// Binding is the credential binding of one authentication profile.
type Binding struct {
	BindingID          string     `json:"binding_id"`
	AuthProfile        string     `json:"auth_profile"`
	CurrentRef         string     `json:"credential_ref"`
	PreviousRef        string     `json:"previous_credential_ref,omitempty"`
	PreviousGraceUntil *time.Time `json:"previous_grace_until,omitempty"`
	GrantedScopes      []string   `json:"granted_scopes"`
	ScopeFingerprint   string     `json:"scope_fingerprint"`
	ExcessScopeAckBy   string     `json:"excess_scope_ack_by,omitempty"`
	Status             string     `json:"status"`
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
	RotationDueAt      *time.Time `json:"rotation_due_at,omitempty"`
	BoundBy            string     `json:"bound_by"`
	BoundAt            time.Time  `json:"bound_at"`
}

// Connection is a tenant's binding to a connector.
type Connection struct {
	ConnectionID        string            `json:"connection_id"`
	TenantID            string            `json:"tenant_id"`
	Environment         string            `json:"environment"`
	ConnectorID         string            `json:"connector_id"`
	ContractVersionID   string            `json:"contract_version_id"`
	Status              string            `json:"status"`
	StatusReason        string            `json:"status_reason,omitempty"`
	Region              string            `json:"region"`
	Endpoints           map[string]string `json:"endpoints"`
	ProviderAccountHash string            `json:"provider_account_hash"`
	ExternalOrgHash     string            `json:"external_org_hash,omitempty"`
	BridgeID            string            `json:"bridge_id,omitempty"`
	ConfigFingerprint   string            `json:"config_fingerprint,omitempty"`
	CapabilitySnapshot  json.RawMessage   `json:"capability_snapshot,omitempty"`
	Bindings            []Binding         `json:"bindings"`
	CreatedBy           string            `json:"created_by"`
	ConsentActor        string            `json:"consent_actor,omitempty"`
	ActivatedBy         string            `json:"activated_by,omitempty"`
	ActivatedAt         *time.Time        `json:"activated_at,omitempty"`
	SuspendedAt         *time.Time        `json:"suspended_at,omitempty"`
	RevokedAt           *time.Time        `json:"revoked_at,omitempty"`
	Version             int64             `json:"version"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

// Check is one validation run or identity probe.
type Check struct {
	CheckID           string           `json:"check_id"`
	Kind              string           `json:"kind"`
	Outcome           string           `json:"outcome"`
	ConfigFingerprint string           `json:"config_fingerprint"`
	Issues            []registry.Issue `json:"issues,omitempty"`
	Detail            json.RawMessage  `json:"detail,omitempty"`
	RunBy             string           `json:"run_by"`
	At                time.Time        `json:"at"`
}

// Certification is one certification run, bound to the contract, mappings, auth profiles and environment it
// covered (I-29).
type Certification struct {
	CertificationID   string    `json:"certification_run_id"`
	ConnectionID      string    `json:"connection_id"`
	Environment       string    `json:"environment"`
	ContractVersionID string    `json:"contract_version_id"`
	ScopeFingerprint  string    `json:"scope_fingerprint"`
	TestPackVersion   string    `json:"test_pack_version"`
	PassedTests       []string  `json:"passed_tests"`
	Outcome           string    `json:"outcome"`
	CertifiedBy       string    `json:"certified_by"`
	At                time.Time `json:"at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// Session is a single-use provider authorization session.
type Session struct {
	SessionID   string     `json:"session_id"`
	StateHash   string     `json:"-"`
	RedirectURI string     `json:"redirect_uri"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ConsumedAt  *time.Time `json:"consumed_at,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Request is a governed request: a contract migration or a break-glass repair.
type Request struct {
	RequestID    string          `json:"request_id"`
	ConnectionID string          `json:"connection_id"`
	Kind         string          `json:"kind"`
	Payload      json.RawMessage `json:"payload"`
	Reason       string          `json:"reason"`
	Status       string          `json:"status"`
	RequestedBy  string          `json:"requested_by"`
	DecidedBy    string          `json:"decided_by,omitempty"`
	DecidedAt    *time.Time      `json:"decided_at,omitempty"`
	EffectiveAt  *time.Time      `json:"effective_at,omitempty"`
	ExpiresAt    *time.Time      `json:"expires_at,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// Event is the append-only evidence of a connection change.
type Event struct {
	Kind   string    `json:"kind"`
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
	Actor  string    `json:"actor"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// OutEvent is a domain event for the transactional outbox.
type OutEvent struct {
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

// Filter narrows a list.
type Filter struct {
	Status      string
	ConnectorID string
	Environment string
}

// Tx is what Mutate hands to its function: the locked connection and everything the decision needs, plus the
// effects to record in the same transaction.
type Tx struct {
	C        *Connection
	Checks   []Check         // newest last
	Certs    []Certification // newest last
	Session  *Session        // the connection's latest authorization session, if any
	Requests []Request       // all requests, oldest first

	NewEvent       *Event
	NewCheck       *Check
	NewCert        *Certification
	NewSession     *Session
	ConsumeSession bool
	NewRequest     *Request
	UpdateRequest  *Request
	ApplyMigration string // contract version id: the pin moves inside this transaction
	Out            []OutEvent
}

// Repo is the persistence boundary of XIC-02.
type Repo interface {
	Create(ctx context.Context, c *Connection, idemKey, idemHash string, ev OutEvent) (created bool, err error)
	Get(ctx context.Context, id string) (*Connection, error)
	// GetAcrossTenants resolves a connection by id regardless of tenant, for the inbound path that finds a
	// connection from a trusted route or credential before it knows the tenant. It reads only.
	GetAcrossTenants(ctx context.Context, id string) (*Connection, error)
	List(ctx context.Context, f Filter) ([]Connection, error)
	Mutate(ctx context.Context, id string, fn func(tx *Tx) error) (*Connection, error)
	ListChecks(ctx context.Context, id string) ([]Check, error)
	ListCertifications(ctx context.Context, id string) ([]Certification, error)
	ListEvents(ctx context.Context, id string) ([]Event, error)
	ListRequests(ctx context.Context, id string) ([]Request, error)
	// Due returns connections whose bindings, certifications or pins need evaluating, across tenants.
	Due(ctx context.Context, now time.Time, window time.Duration, limit int) ([]DueRef, error)
	// RecordSignal writes a security-signal event in its own transaction, so it survives the failure that
	// raised it.
	RecordSignal(ctx context.Context, tenant string, ev OutEvent) error
}

// DueRef names a connection the evaluator should look at.
type DueRef struct{ TenantID, ConnectionID string }

// ── fingerprints ─────────────────────────────────────────────────────────────

func sha(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// HashAccountRef is how a provider account reference is stored: only ever as this salted hash.
func HashAccountRef(tenant, connectorID, ref string) string {
	return sha("account", tenant, connectorID, strings.TrimSpace(ref))
}

// ScopeFingerprint is the fingerprint of a scope set, order-independent.
func ScopeFingerprint(scopes []string) string {
	s := append([]string{}, scopes...)
	sort.Strings(s)
	return sha(append([]string{"scopes"}, s...)...)
}

// ConfigFingerprint covers everything about a connection's configuration that a validation vouches for: the
// pin, environment, region, endpoint bindings and each binding's credential reference and scope set.
func ConfigFingerprint(c *Connection) string {
	parts := []string{"config", c.ConnectionID, c.Environment, c.ContractVersionID, c.Region}
	keys := make([]string, 0, len(c.Endpoints))
	for k := range c.Endpoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, "ep:"+k+"="+c.Endpoints[k])
	}
	bs := append([]Binding{}, c.Bindings...)
	sort.Slice(bs, func(i, j int) bool { return bs[i].AuthProfile < bs[j].AuthProfile })
	for _, b := range bs {
		parts = append(parts, "bind:"+b.AuthProfile+"="+strings.ToLower(b.CurrentRef)+"/"+b.ScopeFingerprint)
	}
	return sha(parts...)
}

// CertFingerprint covers what a certification vouches for (I-29): this connection in this environment against
// exactly this contract content, these mapping transforms and these authentication profiles. A credential
// rotation does not change it; a new contract version, a changed mapping, a changed auth profile or a different
// environment does.
func CertFingerprint(c *Connection, v *registry.ContractVersion) string {
	parts := []string{"cert", c.ConnectionID, c.Environment, v.ContractVersionID, v.ContentHash}
	var maps []string
	for _, m := range v.MappingVersions {
		maps = append(maps, m.Key+"@"+itoa(m.Version)+"="+m.TransformHash)
	}
	sort.Strings(maps)
	parts = append(parts, maps...)
	var auths []string
	for _, p := range v.Content.Profiles {
		if p.Kind == registry.ProfileAuth {
			auths = append(auths, p.Name+"="+sha(string(p.Spec)))
		}
	}
	sort.Strings(auths)
	return sha(append(parts, auths...)...)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// BindingFor returns the binding of an authentication profile.
func (c *Connection) BindingFor(profile string) *Binding {
	for i := range c.Bindings {
		if c.Bindings[i].AuthProfile == profile {
			return &c.Bindings[i]
		}
	}
	return nil
}

// Live says whether the connection can carry exchanges at all.
func (c *Connection) Live() bool { return c.Status == StatusActive || c.Status == StatusDegraded }

// EventTypes lists every event XIC-02 emits (ZS-SVC-T-001 section 14.1: integration.connection.*,
// integration.certification.* and the security signals). A test proves each is produced by a real flow and is a
// valid envelope.
var EventTypes = []string{
	"integration.connection.created", "integration.connection.authorizing", "integration.connection.credential-bound",
	"integration.connection.validated", "integration.connection.validation-failed", "integration.connection.probed",
	"integration.connection.activated", "integration.connection.suspended", "integration.connection.resumed",
	"integration.connection.revoked", "integration.connection.scopes-changed", "integration.connection.credential-rotated",
	"integration.connection.credential-expiring", "integration.connection.credential-expired",
	"integration.connection.migration-requested", "integration.connection.migrated",
	"integration.connection.break-glass-requested", "integration.certification.passed",
	"integration.security.cross-tenant-credential", "integration.security.tenant-scope-mismatch",
	"integration.connection.break-glass-denied",
}

type budgetKey struct{}

// WithoutBudget marks a call to Operable that re-checks work already admitted (a submit of a reserved attempt, a
// status query, a readiness check): the staged rollout still applies its stage and allowlist but takes nothing from
// the pilot's daily budget.
func WithoutBudget(ctx context.Context) context.Context { return context.WithValue(ctx, budgetKey{}, true) }

// BudgetFree reports whether the call carries WithoutBudget.
func BudgetFree(ctx context.Context) bool { v, _ := ctx.Value(budgetKey{}).(bool); return v }
