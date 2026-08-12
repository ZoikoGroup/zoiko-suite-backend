// Package domain defines the authoritative domain types for
// capability-registry-svc.
//
// Per docs/original_doc/zoiko_suite_doc7.txt §7 ("Capability, Module &
// Release Registry"), this service answers five INDEPENDENT questions about
// a capability, each with its own controlling object:
//
//	capability existence        -> capability_registry (this service)
//	commercial entitlement       -> entitlement_service (commercial-account-svc)
//	market availability          -> market_release_registry (this service)
//	integration readiness        -> integration_capability_registry (this service)
//	security/privacy eligibility -> policy engine (policy-svc)
//	operational release state    -> release_registry (this service)
//	execution authority          -> authorization + automation policy (authorization-svc)
//	public claim eligibility     -> claim_registry (this service)
//
// §7's own warning is the reason these stay separate tables rather than one
// feature-flag blob: "A feature can therefore be technically implemented but
// commercially unavailable; commercially entitled but market-restricted;
// market-eligible but disabled by security/incident state... These are
// separate dimensions and must never be compressed into one feature flag."
package domain

import "time"

// Capability is DATA per §B1's doctrine applied here too: CapabilityCode is
// never a code switch/case in this service — only the owning feature's own
// handler would ever branch on it, and this service doesn't implement
// features, only tracks their existence.
type Capability struct {
	CapabilityID         string    `json:"capability_id"`
	CapabilityCode       string    `json:"capability_code"`
	ModuleDomain         string    `json:"module_domain"`
	Version              int       `json:"version"`
	Dependencies         *string   `json:"dependencies,omitempty"` // comma-separated capability_codes
	ExecutionRiskClass   string    `json:"execution_risk_class"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

type CreateCapabilityRequest struct {
	CapabilityCode     string `json:"capability_code"`
	ModuleDomain       string `json:"module_domain"`
	Version            int    `json:"version"`
	Dependencies       string `json:"dependencies,omitempty"`
	ExecutionRiskClass string `json:"execution_risk_class"`
	CorrelationID      string `json:"correlation_id"`
}

// MarketReleaseState is doc7 §29's "Market release" state machine, verbatim.
type MarketReleaseState string

const (
	MarketReleaseInternal   MarketReleaseState = "INTERNAL"
	MarketReleasePilot      MarketReleaseState = "PILOT"
	MarketReleaseBeta       MarketReleaseState = "BETA"
	MarketReleaseGA         MarketReleaseState = "GA"
	MarketReleaseRestricted MarketReleaseState = "RESTRICTED"
	MarketReleaseSuspended  MarketReleaseState = "SUSPENDED"
	MarketReleaseRetired    MarketReleaseState = "RETIRED"
)

// MarketRelease answers "is this capability approved in this market/entity
// jurisdiction and language" (doc7 §7 table, §Q1) — independent of whether
// it's operationally enabled (ReleaseRegistry) or commercially entitled
// (entitlement-service, a different microservice).
type MarketRelease struct {
	MarketReleaseID      string             `json:"market_release_id"`
	CapabilityID         string             `json:"capability_id"`
	MarketCode           string             `json:"market_code"`
	LanguageCode         *string            `json:"language_code,omitempty"`
	LegalApprovalStatus  string             `json:"legal_approval_status"` // APPROVED | PENDING | REJECTED — data
	State                MarketReleaseState `json:"state"`
	EffectiveFrom        time.Time          `json:"effective_from"`
	EffectiveTo          *time.Time         `json:"effective_to,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
	CreatedByPrincipalID string             `json:"created_by_principal_id"`
}

type CreateMarketReleaseRequest struct {
	CapabilityID        string `json:"capability_id"`
	MarketCode          string `json:"market_code"`
	LanguageCode        string `json:"language_code,omitempty"`
	LegalApprovalStatus string `json:"legal_approval_status"`
	State               string `json:"state"`
	EffectiveFrom       string `json:"effective_from"`
	CorrelationID       string `json:"correlation_id"`
}

// IntegrationCapability answers "are required connectors/providers
// certified and healthy" for a capability that depends on an external
// provider (doc7 §7 table).
type IntegrationCapability struct {
	IntegrationCapabilityID string    `json:"integration_capability_id"`
	CapabilityID            string    `json:"capability_id"`
	ProviderCode            string    `json:"provider_code"`
	Certified               bool      `json:"certified"`
	HealthStatus            string    `json:"health_status"` // HEALTHY | DEGRADED | FAILED | UNKNOWN — data, doc7 §29 "Integration health"
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
	CreatedByPrincipalID    string    `json:"created_by_principal_id"`
}

type CreateIntegrationCapabilityRequest struct {
	CapabilityID  string `json:"capability_id"`
	ProviderCode  string `json:"provider_code"`
	Certified     bool   `json:"certified"`
	HealthStatus  string `json:"health_status,omitempty"`
	CorrelationID string `json:"correlation_id"`
}

type UpdateIntegrationHealthRequest struct {
	HealthStatus string `json:"health_status"`
}

// ReleaseState is doc7 §7's "Operational release state" list, verbatim —
// deliberately a different vocabulary from MarketReleaseState (both
// literally quoted from different tables in the same spec section), because
// they are independent dimensions per §7's own warning.
type ReleaseState string

const (
	ReleaseStateGA                 ReleaseState = "GA"
	ReleaseStateBeta               ReleaseState = "BETA"
	ReleaseStatePilot              ReleaseState = "PILOT"
	ReleaseStateInternal           ReleaseState = "INTERNAL"
	ReleaseStateDisabled           ReleaseState = "DISABLED"
	ReleaseStateIncidentRestricted ReleaseState = "INCIDENT_RESTRICTED"
)

// Release is the capability's current operational state — e.g. flipped to
// INCIDENT_RESTRICTED during an incident without touching market approval
// or commercial entitlement at all (doc7 §32.1 kill-switch doctrine).
type Release struct {
	ReleaseID            string       `json:"release_id"`
	CapabilityID         string       `json:"capability_id"`
	State                ReleaseState `json:"state"`
	Reason               *string      `json:"reason,omitempty"`
	EffectiveFrom        time.Time    `json:"effective_from"`
	CreatedAt            time.Time    `json:"created_at"`
	CreatedByPrincipalID string       `json:"created_by_principal_id"`
}

type SetReleaseStateRequest struct {
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	CorrelationID string `json:"correlation_id"`
}

// CapabilityClaim is what marketing/sales may say about a capability's
// availability — doc7 §C2: "Public claim service consumes only claim_registry
// entries linked to release evidence, market scope, wording owner, approval
// and expiry/review date." Roadmap state is never itself a public claim.
type CapabilityClaim struct {
	ClaimID                 string     `json:"claim_id"`
	CapabilityID            string     `json:"capability_id"`
	ClaimText               string     `json:"claim_text"`
	MarketScope             *string    `json:"market_scope,omitempty"`
	WordingOwnerPrincipalID string     `json:"wording_owner_principal_id"`
	ApprovedByPrincipalID   string     `json:"approved_by_principal_id"`
	ExpiryReviewDate        *time.Time `json:"expiry_review_date,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	CreatedByPrincipalID    string     `json:"created_by_principal_id"`
}

type CreateCapabilityClaimRequest struct {
	CapabilityID            string `json:"capability_id"`
	ClaimText               string `json:"claim_text"`
	MarketScope             string `json:"market_scope,omitempty"`
	WordingOwnerPrincipalID string `json:"wording_owner_principal_id"`
	ApprovedByPrincipalID   string `json:"approved_by_principal_id"`
	ExpiryReviewDate        string `json:"expiry_review_date,omitempty"`
	CorrelationID           string `json:"correlation_id"`
}

// CapabilityResolution is the answer to doc7 §C1: "Can the UI infer
// availability from plan alone?" — No. This is the structured reason-code
// response composing this service's OWN three dimensions (capability
// existence, market release, integration readiness, operational release
// state). It deliberately does NOT call out to commercial-account-svc for
// entitlement or policy-svc for security eligibility — those remain the
// caller's own separate checks, per §7's explicit multi-dimension doctrine;
// composing every dimension into one giant cross-service call would be
// exactly the "one feature flag" collapse §7 warns against.
type CapabilityResolution struct {
	CapabilityCode string `json:"capability_code"`
	Enabled        bool   `json:"enabled"`
	ReasonCode     string `json:"reason_code"` // ENABLED | CAPABILITY_UNKNOWN | MARKET_BLOCKED | PROVIDER_UNAVAILABLE | INCIDENT_RESTRICTED | DISABLED
	Detail         string `json:"detail,omitempty"`
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrCapabilityNotFound            = errorString("capability not found")
	ErrMarketReleaseNotFound         = errorString("market release not found")
	ErrIntegrationCapabilityNotFound = errorString("integration capability not found")
	ErrReleaseNotFound               = errorString("release not found")
	ErrClaimNotFound                 = errorString("capability claim not found")
	ErrConflict                      = errorString("conflict: capability_code already exists")
)
