// Package domain defines the authoritative domain types for
// kill-switch-registry-svc.
//
// Per docs/original_doc/zoiko_suite_doc7.txt §32.1: "Kill switch is
// plane/domain/provider/tenant scoped, privileged, logged, approval-
// controlled, visible in operations, and has reconciliation/restart
// procedure. It does not erase already-committed history."
//
// Distinct from capability-registry-svc's Release/ReleaseState: that
// answers "is this ONE capability operationally enabled" (capability_id-
// scoped only). This answers "must this class of action be stopped right
// now" across four independent, combinable scope dimensions at once —
// an incident-response question, not a product-availability one.
package domain

import "time"

// KillSwitchAction is DATA ONLY — no code switch/case in this service
// beyond the store's own append/resolve logic.
type KillSwitchAction string

const (
	KillSwitchActionEngage    KillSwitchAction = "ENGAGE"
	KillSwitchActionDisengage KillSwitchAction = "DISENGAGE"
)

// KillSwitchEvent is one append-only ENGAGE/DISENGAGE record. "Currently
// engaged" for a (Plane, Domain, ProviderCode, TenantID) tuple is always
// DERIVED as that tuple's most recent event — this type is never mutated
// in place once created.
//
// Plane/Domain/ProviderCode/TenantID are independently nullable: nil on
// any of them means "not scoped along that dimension" — e.g. Domain set
// with Plane nil applies across every plane; all four nil is the single
// most severe platform-wide stop.
type KillSwitchEvent struct {
	KillSwitchEventID string  `json:"kill_switch_event_id"`
	Plane             *string `json:"plane,omitempty"`
	Domain            *string `json:"domain,omitempty"`
	ProviderCode      *string `json:"provider_code,omitempty"`
	TenantID          *string `json:"tenant_id,omitempty"`

	Action KillSwitchAction `json:"action"`
	Reason string           `json:"reason"`

	// ReconciliationProcedureRef is required on ENGAGE — doc7's "has
	// reconciliation/restart procedure". A reference to where the runbook
	// lives, not the runbook content itself.
	ReconciliationProcedureRef *string `json:"reconciliation_procedure_ref,omitempty"`

	ApprovedByPrincipalID string    `json:"approved_by_principal_id"`
	CreatedAt             time.Time `json:"created_at"`
	CreatedByPrincipalID  string    `json:"created_by_principal_id"`
}

// EngageKillSwitchRequest is the wire shape for POST /v1/kill-switches/engage.
type EngageKillSwitchRequest struct {
	Plane                      string `json:"plane,omitempty"`
	Domain                     string `json:"domain,omitempty"`
	ProviderCode               string `json:"provider_code,omitempty"`
	TenantID                   string `json:"tenant_id,omitempty"`
	Reason                     string `json:"reason"`
	ReconciliationProcedureRef string `json:"reconciliation_procedure_ref"`
	ApprovedByPrincipalID      string `json:"approved_by_principal_id"`
	CorrelationID              string `json:"correlation_id"`
}

// DisengageKillSwitchRequest is the wire shape for
// POST /v1/kill-switches/disengage. Must name the EXACT scope tuple being
// disengaged — disengage is never itself a fallback/fuzzy match, only
// resolve is.
type DisengageKillSwitchRequest struct {
	Plane                 string `json:"plane,omitempty"`
	Domain                string `json:"domain,omitempty"`
	ProviderCode          string `json:"provider_code,omitempty"`
	TenantID              string `json:"tenant_id,omitempty"`
	Reason                string `json:"reason"`
	ApprovedByPrincipalID string `json:"approved_by_principal_id"`
	CorrelationID         string `json:"correlation_id"`
}

// KillSwitchState is one scope tuple's CURRENT derived state — the latest
// event for that exact tuple, whichever action it was.
type KillSwitchState struct {
	Plane         *string          `json:"plane,omitempty"`
	Domain        *string          `json:"domain,omitempty"`
	ProviderCode  *string          `json:"provider_code,omitempty"`
	TenantID      *string          `json:"tenant_id,omitempty"`
	Action        KillSwitchAction `json:"action"`
	Reason        string           `json:"reason"`
	LatestEventAt time.Time        `json:"latest_event_at"`
}

// KillSwitchResolution is the answer callers actually need before a
// high-impact action: "am I blocked, and if so by which switch." Checks
// every scope tier from most specific to least specific (exact tenant+
// provider match down to the platform-wide tuple) and returns the most
// specific currently-ENGAGED match, if any.
type KillSwitchResolution struct {
	Blocked      bool             `json:"blocked"`
	MatchedEvent *KillSwitchEvent `json:"matched_event,omitempty"`
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	// ErrNotCurrentlyEngaged is returned when a disengage request names a
	// scope tuple whose latest event is already DISENGAGE (or has no
	// events at all) — disengaging a switch that isn't engaged is a no-op
	// request error, not silently accepted as a new fact.
	ErrNotCurrentlyEngaged = errorString("kill switch is not currently engaged for this scope")
)
