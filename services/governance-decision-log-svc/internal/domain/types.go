// Package domain contains the authoritative domain types for
// governance-decision-log-svc.
//
// GovernanceDecision is an append-only evidence record: once written it is
// never updated or deleted (doctrine.md — no soft-delete on material
// objects, evidence is append-only). action_type, outcome, and rule_basis
// are plain strings — data driven, never Go enums or switch/case branches.
package domain

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrDecisionNotFound is returned when a lookup finds no matching row.
var ErrDecisionNotFound = errors.New("governance decision not found")

// ErrStoreUnavailable is returned when the store cannot be reached at all
// (as distinct from a legitimate "not found"). Callers must treat the two
// differently — see jurisdiction-rules-svc's fail-closed precedent.
var ErrStoreUnavailable = errors.New("governance decision store unavailable")

// ErrTenantScopeMissing is returned when a request carries no verified tenant
// scope (no X-Tenant-Id). Every decision is tenant-owned; a write with no scope
// must not fall back to the tenant_id the body named, which is what it did.
var ErrTenantScopeMissing = errors.New("caller tenant scope missing")

// ErrTenantScopeMismatch is returned when a body names a tenant other than the
// caller's verified scope. tenant_id in the body used to be the ONLY source of
// the stored tenant AND of the RLS scope, so a body naming another tenant filed
// the decision in that tenant's log.
var ErrTenantScopeMismatch = errors.New("request tenant_id does not match the caller's verified tenant scope")

// ErrIdempotencyKeyInUse is returned when a concurrent request committed the
// same Idempotency-Key first. The write that lost the race is rolled back whole
// -- decision, key and outbox row -- so the caller re-reads the winner's key and
// answers as a replay or a mismatch.
var ErrIdempotencyKeyInUse = errors.New("idempotency key already used by a concurrent request")

// GovernanceDecision is the durable record of one governance evaluation.
//
// This is the MVP schema (see CONTEXT.md "FINALIZED — MVP schema"): a
// deliberate simplification of the full GovernanceDecision entity in
// docs/architecture/04-data-model.md §7.1. Fields promoted to columns
// (policy_version_id, action_subject_type, action_subject_id) to support
// queryability per Doc 04 §7.1.
type GovernanceDecision struct {
	// DecisionID is caller-supplied and is the idempotency/dedup key.
	DecisionID string `json:"decision_id"`

	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`

	// ActorID is the principal that triggered the evaluated action.
	ActorID string `json:"actor_id"`

	ActionType string `json:"action_type"`

	// Outcome is data-driven, e.g. GRANTED, DENIED, ESCALATED.
	Outcome string `json:"outcome"`

	// RuleBasis references the policy/jurisdiction rule that produced
	// Outcome — doctrine requires basis, not just outcome, to be stored.
	RuleBasis string `json:"rule_basis"`

	// JurisdictionContext is the jurisdiction the decision was made in (the
	// request's X-Jurisdiction-Context). With RuleBasis it is Doc 04 §7.1's
	// jurisdiction_rule_basis. Nil when the request carried none.
	JurisdictionContext *string `json:"jurisdiction_context,omitempty"`

	// PolicyVersionID is the exact policy version that produced this decision.
	// Populated from rule_basis or provided directly by the caller.
	PolicyVersionID *string `json:"policy_version_id,omitempty"`

	// EvaluationContext is a JSONB catch-all for caller-supplied context
	// that doesn't yet have a first-class column. Optional.
	EvaluationContext json.RawMessage `json:"evaluation_context,omitempty"`

	CorrelationID string `json:"correlation_id"`

	// WorkflowInstanceID is nil when this decision was not made in the
	// context of a workflow instance.
	WorkflowInstanceID *string `json:"workflow_instance_id,omitempty"`
	// CausationID is nil when the causing event/decision is not known.
	CausationID *string `json:"causation_id,omitempty"`

	// ActionSubjectType and ActionSubjectId identify the subject of the
	// action (e.g., the entity being acted upon). Per Doc 04 §7.1.
	ActionSubjectType *string `json:"action_subject_type,omitempty"`
	ActionSubjectID   *string `json:"action_subject_id,omitempty"`

	// DecidedAt is when the governance decision was made upstream (by
	// Policy/Authorization/Workflow), not when it was logged here. If the
	// caller omits it, the handler defaults it to server-receipt time.
	// Must be within bounds: not in the future (beyond clock skew) and
	// not before the retention window floor.
	DecidedAt time.Time `json:"decided_at"`

	// RecordedAt is when this service stored the decision (stored_at). It is
	// server-assigned and is the trustworthy time: DecidedAt is the caller's
	// claim, bounded but not verifiable here.
	RecordedAt time.Time `json:"recorded_at"`

	// ── register fields (000016) ─────────────────────────────────────────────
	// All optional on the wire. They describe the decision for a reader; none
	// of them changes what was decided.

	// Title is a one-line name for the decision ("Approve vendor contract renewal").
	Title *string `json:"title,omitempty"`
	// Summary is a short description of what was decided and why.
	Summary *string `json:"summary,omitempty"`
	// Domain is a decision_domains code. Classified from action_type by the
	// database when the caller sends none, so it is always set on a stored row.
	Domain string `json:"domain"`
	// Priority is LOW, MEDIUM, HIGH or CRITICAL; nil when the caller set none.
	Priority *string `json:"priority,omitempty"`
	// Tags are free labels, at most 10.
	Tags []string `json:"tags,omitempty"`
	// RelatedObligationID names an obligations-svc obligation this decision
	// was made under.
	RelatedObligationID *string `json:"related_obligation_id,omitempty"`

	// ── evidence ledger (000016, G-07) ───────────────────────────────────────
	// Server-computed; a caller cannot supply any of them.

	// ChainSeq is this decision's position in its tenant's hash chain (1-based,
	// in storage order).
	ChainSeq int64 `json:"chain_seq"`
	// PrevHash and CurrHash are hex SHA-256 digests: CurrHash =
	// sha256(PrevHash || canonical form of this decision).
	PrevHash string `json:"prev_hash"`
	CurrHash string `json:"curr_hash"`

	// ── read-only, derived when read ─────────────────────────────────────────

	// StatusCategory is APPROVED, PENDING, ESCALATED, DENIED, or OTHER for an
	// outcome no category lists (decision_outcome_categories).
	StatusCategory string `json:"status_category"`
	// DisplayRef is the human reference "GD-<year stored>-<chain_seq>".
	DisplayRef string `json:"display_ref"`
	// FollowUpCount is how many later decisions name this one as their causation.
	FollowUpCount int `json:"follow_up_count"`

	// RetentionClass and RecordClass are constant for every decision (decision
	// D2-2.F11, 11 Nov 2026): EV-REGULATED (Event Catalogue §16: elevated
	// integrity, access and retention controls; legal hold overrides
	// disposition) and the COMPLIANCE_EVIDENCE record class retention-registry-svc
	// resolves the duration for. Set when read, never stored or hashed.
	RetentionClass string `json:"retention_class"`
	RecordClass    string `json:"record_class"`
}

// RetentionClass and RecordClass of every governance decision and of its
// events (decision D2-2.F11, 11 Nov 2026). The duration is not hard-coded
// here: retention-registry-svc resolves it per jurisdiction, record class and
// legal hold (Event Catalogue §16, GOV-09).
const (
	RetentionClass = "EV-REGULATED"
	RecordClass    = "COMPLIANCE_EVIDENCE"
)

// Status categories (decision_outcome_categories.category, plus OTHER).
const (
	CategoryApproved  = "APPROVED"
	CategoryPending   = "PENDING"
	CategoryEscalated = "ESCALATED"
	CategoryDenied    = "DENIED"
	CategoryOther     = "OTHER"
)

// Priorities a decision may carry (000016 chk_priority).
var Priorities = []string{"LOW", "MEDIUM", "HIGH", "CRITICAL"}

// DisplayRef formats the human reference of a decision.
func DisplayRef(recordedAt time.Time, chainSeq int64) string {
	return fmt.Sprintf("GD-%d-%04d", recordedAt.UTC().Year(), chainSeq)
}

// ErrReplayNotSupportedForActionType is returned when a decision's
// action_type has no replay logic implemented — same v1 scope limitation
// as policy-svc's own Evaluate endpoint (APPROVAL_THRESHOLD only), stated
// explicitly rather than silently claiming a replay that didn't happen.
var ErrReplayNotSupportedForActionType = errors.New("replay not implemented for this decision's action_type")

// ErrReplayManifestNotFound is returned when a lookup finds no matching
// replay manifest.
var ErrReplayManifestNotFound = errors.New("replay manifest not found")

// ReplayManifest is doc7's replay-reproducibility record (backlog item
// 34): a permanent statement that decision DecisionID was re-evaluated
// against the EXACT PolicyVersionID it originally used, and whether that
// replay reproduced the OriginalOutcome. Append-only — never updated.
type ReplayManifest struct {
	ReplayManifestID      string    `json:"replay_manifest_id"`
	DecisionID            string    `json:"decision_id"`
	PolicyVersionID       string    `json:"policy_version_id"`
	ReplayedOutcome       string    `json:"replayed_outcome"`
	OriginalOutcome       string    `json:"original_outcome"`
	OutcomesMatch         bool      `json:"outcomes_match"`
	ReplayNotes           *string   `json:"replay_notes,omitempty"`
	ReplayedAt            time.Time `json:"replayed_at"`
	ReplayedByPrincipalID string    `json:"replayed_by_principal_id"`
	TenantID              string    `json:"tenant_id"`

	// IdempotencyKey and RequestHash make a replay retry-safe (000014). They
	// are bookkeeping, not part of the manifest a caller reads.
	IdempotencyKey string `json:"-"`
	RequestHash    []byte `json:"-"`

	// Set only on the tenant-wide replay history (GET /v1/replay-manifests):
	// which decision was replayed, by its human reference and name.
	DecisionDisplayRef string  `json:"decision_display_ref,omitempty"`
	DecisionTitle      *string `json:"decision_title,omitempty"`
	DecisionActionType string  `json:"decision_action_type,omitempty"`
}

// ErrChainBroken is returned when a seal would attest a chain whose links do
// not verify: sealing it would certify tampered evidence.
var ErrChainBroken = errors.New("evidence chain does not verify")

// ErrUnknownDomain is returned when a decision names a domain code
// decision_domains does not hold.
var ErrUnknownDomain = errors.New("unknown decision domain")

// ErrInvalidField is returned when a value violates a column constraint the
// handler did not catch first.
var ErrInvalidField = errors.New("invalid field value")

// PaginationCursor is an opaque cursor for keyset pagination (API-4).
// It encodes the sort key of the last item returned (decided_at DESC, decision_id DESC).
// The cursor is base64.RawURLEncoding encoded JSON.
type PaginationCursor struct {
	// DecidedAt is RFC3339Nano. Second precision dropped the microseconds
	// Postgres keeps, so the keyset comparison skipped or repeated rows.
	DecidedAt  string `json:"decided_at"`
	DecisionID string `json:"decision_id"`
}

// ParseDecidedAt returns the cursor's sort timestamp.
func (c *PaginationCursor) ParseDecidedAt() (time.Time, error) {
	return time.Parse(time.RFC3339Nano, c.DecidedAt)
}

// EncodePaginationCursor encodes a cursor for transport.
func EncodePaginationCursor(c *PaginationCursor) (string, error) {
	if c == nil {
		return "", nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodePaginationCursor decodes a cursor from transport.
func DecodePaginationCursor(s string) (*PaginationCursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c PaginationCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ErrPackageNotFound is returned when a sealed package lookup finds no match.
var ErrPackageNotFound = errors.New("evidence package not found")

// ErrNothingToSeal is returned when a seal would cover no decision the last
// package did not already cover (or the tenant has no decision at all).
var ErrNothingToSeal = errors.New("no unsealed decision to seal")

// ErrSealKeyInUse is returned when a seal's Idempotency-Key was already used
// (by this request or a concurrent one); the caller answers from the stored package.
var ErrSealKeyInUse = errors.New("seal idempotency key already used")

// EvidencePackage is a sealed, append-only snapshot of a tenant's hash chain
// (G-07). MerkleRoot is the RFC 6962 Merkle tree hash over curr_hash of
// decisions 1..ToSeq; PackageHash chains it to the previous package. All hashes
// are hex.
type EvidencePackage struct {
	PackageID       string    `json:"package_id"`
	TenantID        string    `json:"tenant_id"`
	PackageSeq      int       `json:"package_seq"`
	ToSeq           int64     `json:"to_seq"`
	LeafCount       int64     `json:"leaf_count"`
	MerkleRoot      string    `json:"merkle_root"`
	HeadHash        string    `json:"head_hash"`
	HeadDecisionID  string    `json:"head_decision_id"`
	PrevPackageHash string    `json:"prev_package_hash"`
	PackageHash     string    `json:"package_hash"`
	Label           *string   `json:"label,omitempty"`
	Reason          *string   `json:"reason,omitempty"`
	SealedBy        string    `json:"sealed_by"`
	SealedAt        time.Time `json:"sealed_at"`

	// IdempotencyKey and RequestHash make a seal retry-safe; bookkeeping only.
	IdempotencyKey string `json:"-"`
	RequestHash    []byte `json:"-"`
}

// ChainBreak names the first link of a chain that does not verify.
type ChainBreak struct {
	ChainSeq   int64  `json:"chain_seq"`
	DecisionID string `json:"decision_id"`
	// Reason is SEQUENCE_GAP, PREV_HASH_MISMATCH or HASH_MISMATCH.
	Reason string `json:"reason"`
}

// PackageCheck is the result of re-computing one sealed package.
type PackageCheck struct {
	PackageID  string `json:"package_id"`
	PackageSeq int    `json:"package_seq"`
	ToSeq      int64  `json:"to_seq"`
	Valid      bool   `json:"valid"`
	// Reason is empty when valid; else ROOT_MISMATCH, HEAD_MISMATCH,
	// PACKAGE_CHAIN_MISMATCH or LEAVES_MISSING.
	Reason         string `json:"reason,omitempty"`
	RecomputedRoot string `json:"recomputed_root,omitempty"`
}

// ChainVerification is the result of walking a tenant's whole chain.
type ChainVerification struct {
	Valid            bool           `json:"valid"`
	DecisionsChecked int64          `json:"decisions_checked"`
	HeadSeq          int64          `json:"head_seq"`
	HeadHash         string         `json:"head_hash,omitempty"`
	HeadDecisionID   string         `json:"head_decision_id,omitempty"`
	FirstBreak       *ChainBreak    `json:"first_break,omitempty"`
	PackagesChecked  int            `json:"packages_checked"`
	Packages         []PackageCheck `json:"packages"`
	VerifiedAt       time.Time      `json:"verified_at"`
}

// OutboxEvent is one event this service queued for a decision.
type OutboxEvent struct {
	EventType   string     `json:"event_type"`
	CreatedAt   time.Time  `json:"created_at"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	Attempts    int        `json:"attempts"`
	LastError   *string    `json:"last_error,omitempty"`
}

// DecisionEvidence is the evidence held for one decision: its chain link (and
// whether that link re-computes), the package that sealed it, and its events.
type DecisionEvidence struct {
	DecisionID     string           `json:"decision_id"`
	DisplayRef     string           `json:"display_ref"`
	ChainSeq       int64            `json:"chain_seq"`
	PrevHash       string           `json:"prev_hash"`
	CurrHash       string           `json:"curr_hash"`
	HashValid      bool             `json:"hash_valid"`
	PrevLinkValid  bool             `json:"prev_link_valid"`
	Package        *EvidencePackage `json:"package,omitempty"`
	Events         []OutboxEvent    `json:"events"`
	EventsRetained string           `json:"events_note"`
}

// DomainRef is one decision_domains row.
type DomainRef struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// CategoryCounts counts decisions by status category.
type CategoryCounts struct {
	Total     int64 `json:"total"`
	Approved  int64 `json:"approved"`
	Pending   int64 `json:"pending"`
	Escalated int64 `json:"escalated"`
	Denied    int64 `json:"denied"`
	Other     int64 `json:"other"`
}

// Add counts one decision of category c.
func (c *CategoryCounts) Add(category string, n int64) {
	c.Total += n
	switch category {
	case CategoryApproved:
		c.Approved += n
	case CategoryPending:
		c.Pending += n
	case CategoryEscalated:
		c.Escalated += n
	case CategoryDenied:
		c.Denied += n
	default:
		c.Other += n
	}
}

// TrendPoint is one day of the summary's series.
type TrendPoint struct {
	Date string `json:"date"`
	CategoryCounts
}

// NamedCount is a facet value and how many decisions carry it.
type NamedCount struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	Count int64  `json:"count"`
}

// DecisionSummary is GET /v1/decisions/summary: the register's KPIs, panels
// and facets over one window, with the window before it for comparison.
type DecisionSummary struct {
	From           time.Time      `json:"from"`
	To             time.Time      `json:"to"`
	PreviousFrom   time.Time      `json:"previous_from"`
	Totals         CategoryCounts `json:"totals"`
	PreviousTotals CategoryCounts `json:"previous_totals"`
	// OpenPending and OpenEscalated are decisions in those categories that no
	// later decision has followed up (causation_id).
	OpenPending    int64        `json:"open_pending"`
	OpenEscalated  int64        `json:"open_escalated"`
	ByDomain       []NamedCount `json:"by_domain"`
	ByJurisdiction []NamedCount `json:"by_jurisdiction"`
	ByPriority     []NamedCount `json:"by_priority"`
	TopSources     []NamedCount `json:"top_sources"`
	Trend          []TrendPoint `json:"trend"`
	Replays        struct {
		Total      int64 `json:"total"`
		Mismatches int64 `json:"mismatches"`
	} `json:"replays"`
	Chain struct {
		Length        int64      `json:"length"`
		HeadHash      string     `json:"head_hash,omitempty"`
		Packages      int64      `json:"packages"`
		SealedThrough int64      `json:"sealed_through"`
		LastSealedAt  *time.Time `json:"last_sealed_at,omitempty"`
	} `json:"chain"`
	Outbox struct {
		Pending              int64   `json:"pending"`
		Published            int64   `json:"published"`
		OldestPendingSeconds float64 `json:"oldest_pending_seconds"`
	} `json:"outbox"`
	Facets struct {
		Domains       []DomainRef `json:"domains"`
		Jurisdictions []string    `json:"jurisdictions"`
		Priorities    []string    `json:"priorities"`
		Categories    []string    `json:"categories"`
	} `json:"facets"`
}
