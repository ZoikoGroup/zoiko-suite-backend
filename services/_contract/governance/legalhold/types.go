package legalhold

import (
	"time"

	"zoiko.io/contract/types"
)

// HoldStatus defines the lifecycle of a legal preservation directive (§16, DG-036..DG-039).
type HoldStatus string

const (
	HoldStatusActive   HoldStatus = "ACTIVE"
	HoldStatusReleased HoldStatus = "RELEASED"
)

// LegalHold models an authorized preservation hold directive (§16, §28, DG-036).
type LegalHold struct {
	HoldID                 types.UUID `json:"hold_id"`
	TenantID               types.UUID `json:"tenant_id"`
	HoldMatterCode         string     `json:"hold_matter_code"` // e.g. "LIT_2026_DISPUTE_042"
	AuthorityDescription   string     `json:"authority_description"`
	IssuedBy               string     `json:"issued_by"`
	IssuedAt               time.Time  `json:"issued_at"`
	Status                 HoldStatus `json:"status"`
	ReleasedBy             *string    `json:"released_by,omitempty"`
	ReleaseApprovedBy      *string    `json:"release_approved_by,omitempty"` // SoD: Must be distinct authorized approver (NP-16)
	ReleasedAt             *time.Time `json:"released_at,omitempty"`
	ReleaseJustification   *string    `json:"release_justification,omitempty"`
}

// LegalHoldScope models the versioned query/predicate scope of a legal hold (§16, §28, DG-037, DG-038, NP-14).
type LegalHoldScope struct {
	ScopeID               types.UUID `json:"scope_id"`
	TenantID              types.UUID `json:"tenant_id"`
	HoldID                types.UUID `json:"hold_id"`
	Version               int        `json:"version"` // Incremented on scope amendment (DG-038, NP-14)
	TargetEntityTypes     []string   `json:"target_entity_types"`     // e.g. ["SALES_INVOICE", "EMAIL", "JOURNAL_LINE"]
	CustodianPrincipalIDs []string   `json:"custodian_principal_ids"` // e.g. ["usr_cfo", "usr_controller"]
	DateRangeStart        *time.Time `json:"date_range_start,omitempty"`
	DateRangeEnd          *time.Time `json:"date_range_end,omitempty"`
	IsProspective         bool       `json:"is_prospective"`          // DG-037, NP-15: Automatically captures future matching records
	FilterPredicate       string     `json:"filter_predicate,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	CreatedBy             string     `json:"created_by"`
}

// RecordCandidate represents a record being checked against legal hold coverage.
type RecordCandidate struct {
	RecordID    types.UUID
	TenantID    types.UUID
	EntityType  string
	CustodianID string
	RecordDate  time.Time
}
