package domain

import (
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// Stable error catalogue  (Governance Control Plane spec, section 16)
//
// The spec names sixteen stable error classes for the whole control plane and
// requires that a refusal carry one. They are string constants rather than
// Go errors because they cross the wire: a client branches on the code, and a
// code that changed with a refactor would break that branch silently.
//
// Only the classes this service can actually produce are declared. Inventing
// constants for RETENTION_BLOCKED-style outcomes this service never reaches
// would suggest a control that is not here.
// ---------------------------------------------------------------------------

const (
	// ErrCodeContextUnresolved covers every refusal where the trusted context
	// could not be established: no token, an unverifiable one, an inactive
	// principal or tenant, an unauthorized entity.
	ErrCodeContextUnresolved = "CONTEXT_UNRESOLVED"

	// ErrCodeResidencyDenied is returned when the entity's data residency
	// policy is not servable from the region this instance runs in.
	ErrCodeResidencyDenied = "RESIDENCY_DENIED"

	// ErrCodeAuthorizationDenied mirrors a DENIED decision from GOV-03.
	ErrCodeAuthorizationDenied = "AUTHORIZATION_DENIED"

	// ErrCodeSoDConflict mirrors a conflict decision from GOV-04.
	ErrCodeSoDConflict = "SOD_CONFLICT"

	// ErrCodeBreakGlassRequired is returned when a support-scoped read is
	// attempted with no live support context.
	ErrCodeBreakGlassRequired = "BREAK_GLASS_REQUIRED"

	// ErrCodeBreakGlassExpired distinguishes an elapsed grant from an absent
	// one. The two are one answer to the caller in terms of access, but an
	// operator debugging a failed support session needs them apart.
	ErrCodeBreakGlassExpired = "BREAK_GLASS_EXPIRED"

	// ErrCodeLegalHoldActive is returned when disposition is refused.
	ErrCodeLegalHoldActive = "LEGAL_HOLD_ACTIVE"

	// ErrCodeIdempotencyMismatch is returned when an idempotency key is
	// replayed with a different request body.
	ErrCodeIdempotencyMismatch = "IDEMPOTENCY_MISMATCH"

	// ErrCodeTrustPostureBlocked is this service's own addition to the
	// catalogue. The spec's list is control-plane-wide and has no class for a
	// risk-blocked session, which is a GOV-01-specific outcome.
	ErrCodeTrustPostureBlocked = "TRUST_POSTURE_BLOCKED"

	// ErrCodeUpstreamUnavailable reports that no decision could be reached,
	// which is categorically different from a decision to refuse.
	ErrCodeUpstreamUnavailable = "UPSTREAM_UNAVAILABLE"

	// ErrCodeUnsupported is returned for an input the service will never
	// process, such as a SAML assertion with no configured IdP. Distinct from
	// CONTEXT_UNRESOLVED because no retry can succeed.
	ErrCodeUnsupported = "UNSUPPORTED"
)

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

// Environment is the deployment tier a context was resolved in. It is part of
// TenantContextDecision (spec section 18) and is recorded on every session so a
// decision can never be replayed across tiers without that being visible.
type Environment string

const (
	EnvironmentLocal       Environment = "local"
	EnvironmentDevelopment Environment = "development"
	EnvironmentStaging     Environment = "staging"
	EnvironmentProduction  Environment = "production"
)

// Valid reports whether e is one of the four tiers the schema CHECK permits.
// Anything else would be rejected by Postgres at write time, which is far too
// late: the session has already been issued by then.
func (e Environment) Valid() bool {
	switch e {
	case EnvironmentLocal, EnvironmentDevelopment, EnvironmentStaging, EnvironmentProduction:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Ingress
// ---------------------------------------------------------------------------

// IngressUnknown is the recorded ingress_source for a request that presented
// no host or canonical ingress identifier.
//
// A literal value rather than an empty string, for the same reason
// risk_signal_source records UNAVAILABLE: "we did not observe one" and "we
// forgot to record one" must not look identical in the evidence.
const IngressUnknown = "UNKNOWN"

// TenantIngressBinding maps a canonical ingress identifier to the tenant it
// serves. It is a CACHE of a tenant-registry fact, never the master.
//
// The asymmetry is the whole design: a binding can CONTRADICT a token's tenant
// claim, which is a refusal, but it can never supply a tenant the token did
// not claim. That is what keeps GOV-01 out of tenant master data while still
// letting it satisfy negative path #2.
type TenantIngressBinding struct {
	IngressIdentifier string      `json:"ingress_identifier"`
	TenantID          string      `json:"tenant_id"`
	Environment       Environment `json:"environment"`
	ActiveFlag        bool        `json:"active_flag"`
	SourceVersion     string      `json:"source_version"`
	RefreshedAt       time.Time   `json:"refreshed_at"`
}

// ErrIngressTenantMismatch is returned when the ingress a request arrived on
// is bound to a different tenant than the one its token claims.
//
// This is the "spoofed tenant" and "unknown hostname" negative paths meeting
// in one check. It is fail-closed and deliberately not recoverable by the
// caller: there is no legitimate request that arrives on tenant A's hostname
// bearing tenant B's token.
var ErrIngressTenantMismatch = errors.New("ingress is bound to a different tenant than the presented token claims")

// ---------------------------------------------------------------------------
// Residency
// ---------------------------------------------------------------------------

// ErrResidencyDenied is returned when the legal entity's data residency policy
// is not one this deployment region is permitted to serve.
//
// The policy id has been recorded on every SessionContext since 000005, but it
// was only ever WRITTEN — nothing compared it against where the process is
// actually running. A residency policy that is recorded and not enforced
// documents the breach rather than preventing it.
var ErrResidencyDenied = errors.New("legal entity residency policy is not servable from this deployment region")

// ---------------------------------------------------------------------------
// Support context  (AttachSupportContext)
// ---------------------------------------------------------------------------

// SupportContext is a scoped, time-limited, independently-approved elevation
// permitting a support principal to operate inside a customer tenant.
//
// It is NOT a role and grants no permissions of its own. What it does is make
// a support principal's session resolvable in a tenant it does not belong to,
// with every action taken under it tagged with SupportContextID. Authorization
// still runs, unchanged, on every request.
type SupportContext struct {
	SupportContextID   string `json:"support_context_id"`
	TenantID           string `json:"tenant_id"`
	SupportPrincipalID string `json:"support_principal_id"`
	// SubjectPrincipalID narrows the grant to one principal's data. nil is a
	// tenant-wide support context, which is the broader and rarer grant.
	SubjectPrincipalID  *string   `json:"subject_principal_id"`
	ReasonCode          string    `json:"reason_code"`
	Justification       string    `json:"justification"`
	TicketRef           string    `json:"ticket_ref"`
	ApproverPrincipalID string    `json:"approver_principal_id"`
	GrantedAt           time.Time `json:"granted_at"`
	ExpiresAt           time.Time `json:"expires_at"`
	// RevokedAt and RevocationReason are append-only, set at most once.
	RevokedAt        *time.Time `json:"revoked_at"`
	RevocationReason *string    `json:"revocation_reason"`
	// ReviewedAt and ReviewedBy record the post-hoc reconciliation the
	// invariant requires. An expired grant that is never reviewed is a control
	// that ran but was never checked, so the reconciler reports on these.
	ReviewedAt    *time.Time `json:"reviewed_at"`
	ReviewedBy    *string    `json:"reviewed_by"`
	EvidenceID    string     `json:"evidence_id"`
	CorrelationID string     `json:"correlation_id"`
}

// Live reports whether the grant is usable at t: granted, not revoked, not
// expired. All three are checked here rather than at each call site, because
// a call site that forgot one would fail open.
func (s *SupportContext) Live(t time.Time) bool {
	if s == nil {
		return false
	}
	if s.RevokedAt != nil {
		return false
	}
	if !t.Before(s.ExpiresAt) {
		return false
	}
	return !t.Before(s.GrantedAt)
}

// Covers reports whether the grant extends to principalID. A tenant-wide
// grant covers everyone in its tenant; a narrowed one covers only its subject.
func (s *SupportContext) Covers(principalID string) bool {
	if s == nil {
		return false
	}
	if s.SubjectPrincipalID == nil {
		return true
	}
	return *s.SubjectPrincipalID == principalID
}

// SupportReasonCode values. A free-text justification is required alongside,
// but the code is what the reconciliation report groups by, so it is closed.
const (
	SupportReasonIncident       = "INCIDENT_RESPONSE"
	SupportReasonCustomerTicket = "CUSTOMER_TICKET"
	SupportReasonDataCorrection = "DATA_CORRECTION"
	SupportReasonAudit          = "AUDIT_REQUEST"
)

// ValidSupportReason reports whether code is one of the four recognised
// reasons. An unrecognised reason is refused rather than recorded, because a
// reason nobody can group by is not a reason.
func ValidSupportReason(code string) bool {
	switch code {
	case SupportReasonIncident, SupportReasonCustomerTicket,
		SupportReasonDataCorrection, SupportReasonAudit:
		return true
	}
	return false
}

// Support-context sentinel errors.
var (
	// ErrSupportContextNotFound covers absent, foreign-tenant and revoked
	// alike, for the same non-enumeration reason as ErrSessionNotFound.
	ErrSupportContextNotFound = errors.New("support context not found")

	// ErrSupportContextExpired is distinct from not-found so an operator can
	// tell an elapsed grant from one that never existed.
	ErrSupportContextExpired = errors.New("support context expired")

	// ErrSupportSelfApproval is returned when the grantee approved their own
	// elevation. Checked in the handler as well as by a schema CHECK.
	ErrSupportSelfApproval = errors.New("support context requires an independent approver")

	// ErrSupportTTLExceeded is returned when a requested window exceeds the
	// configured maximum. A caller asking for a year is refused rather than
	// silently clamped: clamping would issue a grant nobody asked for and
	// nobody reviewed.
	ErrSupportTTLExceeded = errors.New("requested support window exceeds the maximum permitted TTL")
)

// ---------------------------------------------------------------------------
// Legal hold projection  (READ MODEL of GOV-10)
// ---------------------------------------------------------------------------

// LegalHold is this service's local, read-only view of a GOV-10 hold. It
// exists for exactly one purpose: to refuse disposition.
//
// There is no Issue or Release method anywhere in this service. Rows arrive
// from GOV-10's events and nothing else, which is what keeps the authority
// matrix's "must never own: legal-hold matter" true in code rather than only
// in the document.
type LegalHold struct {
	HoldID    string `json:"hold_id"`
	TenantID  string `json:"tenant_id"`
	MatterRef string `json:"matter_ref"`
	// PrincipalID nil means the hold covers the whole tenant.
	PrincipalID *string    `json:"principal_id"`
	IssuedAt    time.Time  `json:"issued_at"`
	ReleasedAt  *time.Time `json:"released_at"`
	LastEventID string     `json:"last_event_id"`
	LastEventAt time.Time  `json:"last_event_at"`
}

// ErrLegalHoldActive is returned when a disposition is refused because a hold
// covers the record.
var ErrLegalHoldActive = errors.New("disposition blocked by an active legal hold")

// ---------------------------------------------------------------------------
// Retention  (GOV-09)
// ---------------------------------------------------------------------------

// Retention classes owned by this service. The period for each is configured,
// not hard-coded, because a jurisdiction pack can lengthen it and a shortened
// period must never be applied retroactively to rows already written.
const (
	RetentionClassSessionEvidence = "IDENTITY_SESSION_EVIDENCE"
	RetentionClassAccessEvidence  = "IDENTITY_ACCESS_EVIDENCE"
	RetentionClassOutbox          = "IDENTITY_OUTBOX"
)

// DispositionOutcome is what the retention worker did to one batch.
type DispositionOutcome struct {
	RecordClass string `json:"record_class"`
	// Considered is how many rows were due.
	Considered int `json:"considered"`
	// Disposed is how many were actually disposed.
	Disposed int `json:"disposed"`
	// HeldBack is how many were due but blocked by a legal hold. This is the
	// number that matters: a sweep reporting only its successes would make a
	// hold that blocked everything look like a quiet night.
	HeldBack int `json:"held_back"`
	// CertificateID is the evidence object attesting this sweep. GOV-09's
	// contract names a disposition certificate as a required output.
	CertificateID string    `json:"certificate_id"`
	SweptAt       time.Time `json:"swept_at"`
}

// ---------------------------------------------------------------------------
// Atomic evidence
// ---------------------------------------------------------------------------

// ContextResolvedEvent is the identity.context.resolved payload, carried
// alongside the SessionContext so both can be written in one transaction.
//
// A struct rather than the six loose strings the publisher method takes,
// because it travels from the resolver through the session cache to the store,
// and six positional strings surviving three hops without one being
// transposed is not a bet worth taking.
type ContextResolvedEvent struct {
	PrincipalID      string
	TenantID         string
	LegalEntityID    string
	SessionContextID string
	EvidenceID       string
	CorrelationID    string
}

// ---------------------------------------------------------------------------
// Context explanation  (ExplainContextResolution)
// ---------------------------------------------------------------------------

// ContextExplanation answers "why did this context resolve the way it did",
// reconstructed from the recorded decision rather than re-derived.
//
// Re-deriving would read TODAY's risk signals, role assignments and entity
// state against a session issued last week, which is precisely the mistake
// DoD gate 6 ("historical version/as-of reconstruction") exists to prevent.
// Every field here is read from the frozen session_contexts row.
type ContextExplanation struct {
	SessionContextID string      `json:"session_context_id"`
	DecisionID       string      `json:"decision_id"`
	EvidenceID       string      `json:"evidence_id"`
	Outcome          string      `json:"outcome"`
	PrincipalID      string      `json:"principal_id"`
	TenantID         string      `json:"tenant_id"`
	LegalEntityID    string      `json:"legal_entity_id"`
	Environment      Environment `json:"environment"`
	IngressSource    string      `json:"ingress_source"`

	// Dimensions is the per-dimension account: what each of the six resolved
	// to, and from which source. This is the part an auditor reads.
	Dimensions []DimensionOutcome `json:"dimensions"`

	IssuedAt           time.Time  `json:"issued_at"`
	ExpiresAt          time.Time  `json:"expires_at"`
	InvalidatedAt      *time.Time `json:"invalidated_at,omitempty"`
	InvalidationReason *string    `json:"invalidation_reason,omitempty"`
	SupportContextID   *string    `json:"support_context_id,omitempty"`

	// AsOf is the instant the reconstruction was requested for. It echoes the
	// caller's as_of, or the time of the call when none was given.
	AsOf time.Time `json:"as_of"`

	// ReconstructedFrom names the evidence the answer was built from, so a
	// reader can tell a recorded decision from a live re-derivation.
	ReconstructedFrom string `json:"reconstructed_from"`

	CorrelationID string `json:"correlation_id"`
	SchemaVersion string `json:"schema_version"`
}

// DimensionOutcome is one of the six dimensions' recorded result.
type DimensionOutcome struct {
	Dimension int    `json:"dimension"`
	Name      string `json:"name"`
	Result    string `json:"result"`
	// Source names where the value came from: the token, this service's own
	// store, an upstream registry, or the async risk cache. An auditor asking
	// "who asserted this" gets the answer here.
	Source string `json:"source"`
	// Detail is human-readable and deliberately not machine-parsed.
	Detail string `json:"detail,omitempty"`
}

// ---------------------------------------------------------------------------
// Wire types for the GOV-01 commands and queries added in this change
// ---------------------------------------------------------------------------

// ExplainContextRequest asks for the recorded account of one resolution.
type ExplainContextRequest struct {
	SessionContextID string `json:"session_context_id"`
	// AsOf reconstructs the decision as it stood at an instant. Zero means
	// "as recorded", which for an append-only row is the same answer.
	AsOf          *time.Time `json:"as_of,omitempty"`
	CorrelationID string     `json:"correlation_id,omitempty"`
}

// RefreshCacheRequest asks GOV-01 to drop its cached routing hints so the next
// resolution re-reads them from the registry.
//
// Scope is the tenant the caller is verified for. There is no "all tenants"
// form: a single caller invalidating the entire estate's context cache is a
// denial-of-service primitive wearing a maintenance command's clothes.
type RefreshCacheRequest struct {
	// IngressIdentifiers optionally narrows the refresh. Empty refreshes every
	// binding for the caller's tenant.
	IngressIdentifiers []string `json:"ingress_identifiers,omitempty"`
	Reason             string   `json:"reason"`
	CorrelationID      string   `json:"correlation_id,omitempty"`
}

// RefreshCacheResponse reports what was dropped.
type RefreshCacheResponse struct {
	BindingsRefreshed int    `json:"bindings_refreshed"`
	EvidenceID        string `json:"evidence_id"`
}

// InvalidateTenantContextRequest revokes every live session in a tenant.
//
// This is the tenant-wide counterpart to the per-session invalidate, and it is
// the command the spec names. It is deliberately a separate route with its own
// authorization action rather than a flag on the session route: "log one user
// out" and "log an entire tenant out" are not the same decision and must not
// share a permission.
type InvalidateTenantContextRequest struct {
	Reason        InvalidationReason `json:"reason"`
	Justification string             `json:"justification"`
	CorrelationID string             `json:"correlation_id,omitempty"`
}

// InvalidateTenantContextResponse reports the blast radius.
type InvalidateTenantContextResponse struct {
	SessionsRevoked int    `json:"sessions_revoked"`
	EvidenceID      string `json:"evidence_id"`
}

// AttachSupportContextRequest is the privileged elevation request.
//
// ApproverPrincipalID is mandatory and must differ from the grantee. There is
// no single-party form of this command.
type AttachSupportContextRequest struct {
	TenantID            string  `json:"tenant_id"`
	SupportPrincipalID  string  `json:"support_principal_id"`
	SubjectPrincipalID  *string `json:"subject_principal_id,omitempty"`
	ReasonCode          string  `json:"reason_code"`
	Justification       string  `json:"justification"`
	TicketRef           string  `json:"ticket_ref"`
	ApproverPrincipalID string  `json:"approver_principal_id"`
	// TTLSeconds is the requested window. Refused outright if it exceeds the
	// configured maximum rather than clamped — see ErrSupportTTLExceeded.
	TTLSeconds    int    `json:"ttl_seconds"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// AttachSupportContextResponse returns the grant and its evidence.
type AttachSupportContextResponse struct {
	SupportContextID string    `json:"support_context_id"`
	ExpiresAt        time.Time `json:"expires_at"`
	EvidenceID       string    `json:"evidence_id"`
}

// RevokeSupportContextRequest ends a grant early.
type RevokeSupportContextRequest struct {
	Reason        string `json:"reason"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// ResolveResponseV2 extends ResolveResponse with the evidence id the spec's
// envelope section requires on a material governance decision.
//
// Kept as a distinct type rather than adding the field to ResolveResponse so
// the change is visible at every construction site. The JSON is a superset,
// so an existing client reading only envelope_jwt is unaffected.
type ResolveResponseV2 struct {
	EnvelopeJWT      string `json:"envelope_jwt"`
	EvidenceID       string `json:"evidence_id"`
	SessionContextID string `json:"session_context_id"`
	ExpiresAt        int64  `json:"expires_at"`
}
