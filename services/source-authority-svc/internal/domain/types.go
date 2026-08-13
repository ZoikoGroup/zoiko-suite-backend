// Package domain defines the authoritative domain types for
// source-authority-svc.
//
// Per docs/original_doc/zoiko_suite_doc7.txt §D1-D3, §K1: this service
// answers "which connected system's value should I trust for this field,
// right now" — never silently guessing when sources disagree at the same
// precedence tier (§D2: "Ambiguous material facts block downstream
// high-impact decisions"). field_family, source_system, and
// authority_class are all plain strings, DATA ONLY, no code switch/case.
package domain

import (
	"encoding/json"
	"time"
)

// SourceAuthorityMap is a versioned precedence rule: for a given field
// family, how one source system ranks against others. Immutable once
// created — a changed precedence is a new row, never an UPDATE.
type SourceAuthorityMap struct {
	SourceAuthorityMapID  string     `json:"source_authority_map_id"`
	FieldFamily           string     `json:"field_family"`
	SourceSystem          string     `json:"source_system"`
	PrecedenceRank        int        `json:"precedence_rank"`
	ConflictRoute         string     `json:"conflict_route"`
	AllowedCorrectionPath *string    `json:"allowed_correction_path,omitempty"`
	EffectiveFrom         time.Time  `json:"effective_from"`
	EffectiveTo           *time.Time `json:"effective_to,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	CreatedByPrincipalID  string     `json:"created_by_principal_id"`
}

// CreateSourceAuthorityMapRequest is the wire shape for
// POST /v1/source-authority-maps.
type CreateSourceAuthorityMapRequest struct {
	FieldFamily           string `json:"field_family"`
	SourceSystem          string `json:"source_system"`
	PrecedenceRank        int    `json:"precedence_rank"`
	ConflictRoute         string `json:"conflict_route"`
	AllowedCorrectionPath string `json:"allowed_correction_path,omitempty"`
	EffectiveFrom         string `json:"effective_from"`
	CorrelationID         string `json:"correlation_id"`
}

// NormalizedFact is one fact as reported by one source system at one
// point in time — append-only. A correction is a NEW fact with a later
// ObservedAt, never an UPDATE to an existing row (doc7 §D1: "never
// silently back-write").
type NormalizedFact struct {
	NormalizedFactID      string          `json:"normalized_fact_id"`
	FieldFamily           string          `json:"field_family"`
	EntityRef             string          `json:"entity_ref"`
	SourceSystem          string          `json:"source_system"`
	SourceRecord          string          `json:"source_record"`
	SourceVersion         *string         `json:"source_version,omitempty"`
	FactValue             json.RawMessage `json:"fact_value"`
	ObservedAt            time.Time       `json:"observed_at"`
	EffectiveAt           time.Time       `json:"effective_at"`
	TransformationVersion *string         `json:"transformation_version,omitempty"`
	AuthorityClass        string          `json:"authority_class"` // AUTHORITATIVE | DERIVED | CACHED
	CreatedAt             time.Time       `json:"created_at"`
	CreatedByPrincipalID  string          `json:"created_by_principal_id"`
}

// RecordFactRequest is the wire shape for POST /v1/normalized-facts.
type RecordFactRequest struct {
	FieldFamily           string          `json:"field_family"`
	EntityRef             string          `json:"entity_ref"`
	SourceSystem          string          `json:"source_system"`
	SourceRecord          string          `json:"source_record"`
	SourceVersion         string          `json:"source_version,omitempty"`
	FactValue             json.RawMessage `json:"fact_value"`
	ObservedAt            string          `json:"observed_at"`
	EffectiveAt           string          `json:"effective_at,omitempty"` // defaults to observed_at
	TransformationVersion string          `json:"transformation_version,omitempty"`
	AuthorityClass        string          `json:"authority_class,omitempty"` // defaults to AUTHORITATIVE
	CorrelationID         string          `json:"correlation_id"`
}

// FactResolution is the answer to "which value should I trust for this
// field, right now" — composing precedence with conflict detection.
// Ambiguous is a DIFFERENT, deliberately distinct state from a resolved
// value: two equally-ranked sources disagreeing must never be silently
// resolved by picking one arbitrarily (doc7 §D2).
type FactResolution struct {
	FieldFamily       string           `json:"field_family"`
	EntityRef         string           `json:"entity_ref"`
	Ambiguous         bool             `json:"ambiguous"`
	AuthoritativeFact *NormalizedFact  `json:"authoritative_fact,omitempty"`
	ConflictingFacts  []NormalizedFact `json:"conflicting_facts,omitempty"`
	ConflictRoute     *string          `json:"conflict_route,omitempty"`
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var ErrConflict = errorString("conflict: source_authority_map already exists for this field_family+source_system+effective_from")
