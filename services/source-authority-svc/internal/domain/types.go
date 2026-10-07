// Package domain defines the authoritative domain types for
// source-authority-svc.
//
// Per docs/original_doc/zoiko_suite_doc7.txt §D1-D3, §K1: this service
// answers "which connected system's value should I trust for this field,
// right now" — never silently guessing when sources disagree at the same
// precedence tier (§D2: "Ambiguous material facts block downstream
// high-impact decisions"). field_family, source_system, and
// authority_class are all plain strings, DATA ONLY, no code switch/case.
//
// ── the tenant boundary runs between the two tables ─────────────────────────
//
// SourceAuthorityMap is platform-wide reference data and carries no tenant.
// "ADP outranks the HR spreadsheet for PAYROLL_GROSS_PAY" says which connected
// SYSTEM is trusted; it is not about anyone's payroll.
//
// NormalizedFact is tenant business data and carries TenantID. A fact is one
// value about one business entity — an employee's gross pay, a counterparty's
// billing contact — and the original schema gave it no tenant column at all, so
// every tenant's facts shared one pool and Resolve read across all of them.
// The distinction is the whole of migration 000002.
package domain

import (
	"encoding/json"
	"time"
)

// AuthorityClass is the §K1 vocabulary. DATA ONLY in the sense that this
// service does not branch on the value — but the SET is closed, and it was
// previously unenforced in Go and in the schema alike, so a misspelling was
// stored as a class no reader knows how to interpret.
const (
	AuthorityClassAuthoritative = "AUTHORITATIVE"
	AuthorityClassDerived       = "DERIVED"
	AuthorityClassCached        = "CACHED"
)

// ValidAuthorityClass reports whether c is one of the three §K1 classes.
func ValidAuthorityClass(c string) bool {
	switch c {
	case AuthorityClassAuthoritative, AuthorityClassDerived, AuthorityClassCached:
		return true
	default:
		return false
	}
}

// SourceAuthorityMap is a versioned precedence rule: for a given field
// family, how one source system ranks against others.
//
// The rule itself is immutable — precedence_rank, conflict_route and the rest
// are never rewritten. What CAN change is how long it applies: EffectiveTo
// end-dates it. That column existed from the first migration and the resolver
// always honoured it, but nothing could set it, so the documented way to change
// a precedence ("a changed precedence is a new row") left both rows in force
// at once. Supersede is the operation that closes the old window.
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
	CorrelationID         *string    `json:"correlation_id,omitempty"`

	// SupersededAt/SupersededByPrincipalID record who ended the rule and when,
	// which EffectiveTo alone does not: a rule may be created already carrying
	// a planned end date, and that is a different fact from one an operator cut
	// short.
	SupersededAt            *time.Time `json:"superseded_at,omitempty"`
	SupersededByPrincipalID *string    `json:"superseded_by_principal_id,omitempty"`
}

// Superseded reports whether the rule has been explicitly ended by an operator,
// as distinct from merely having passed a planned EffectiveTo.
func (m SourceAuthorityMap) Superseded() bool { return m.SupersededAt != nil }

// EffectiveAt reports whether the rule is in force at t.
func (m SourceAuthorityMap) EffectiveAt(t time.Time) bool {
	if m.EffectiveFrom.After(t) {
		return false
	}
	return m.EffectiveTo == nil || m.EffectiveTo.After(t)
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

// SupersedeSourceAuthorityMapRequest is the wire shape for
// POST /v1/source-authority-maps/{id}/supersede.
//
// EffectiveTo is optional and defaults to now. A future value schedules the
// end of the rule; a past one is refused, because back-dating the window a
// decision was made under rewrites the explanation for decisions already taken
// — doc7 §D1's "never silently back-write", applied to the precedence rules
// rather than to the facts.
type SupersedeSourceAuthorityMapRequest struct {
	EffectiveTo   string `json:"effective_to,omitempty"`
	CorrelationID string `json:"correlation_id"`
}

// ListSourceAuthorityMapsFilter is the parameter object for a precedence read.
type ListSourceAuthorityMapsFilter struct {
	FieldFamily  string
	SourceSystem string

	// IncludeSuperseded widens the read from "the rules in force now" to the
	// full history of the field family. The default is the narrow read, because
	// a list mixing live and ended rules with no way to tell them apart is how
	// an operator concludes a source is ranked twice.
	IncludeSuperseded bool

	Limit  int
	Offset int
}

// NormalizedFact is one fact as reported by one source system at one
// point in time — append-only. A correction is a NEW fact with a later
// ObservedAt, never an UPDATE to an existing row (doc7 §D1: "never
// silently back-write").
type NormalizedFact struct {
	NormalizedFactID      string          `json:"normalized_fact_id"`
	TenantID              string          `json:"tenant_id"`
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
	CorrelationID         *string         `json:"correlation_id,omitempty"`
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

// ListNormalizedFactsFilter is the parameter object for a fact-history read —
// every observation behind a resolution, which is what makes a resolution
// explainable rather than merely asserted.
type ListNormalizedFactsFilter struct {
	TenantID     string
	FieldFamily  string
	EntityRef    string
	SourceSystem string
	Limit        int
	Offset       int
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

	// UnmappedSources names the source systems that HAVE reported a fact for
	// this (field_family, entity_ref) and have no precedence rule in force.
	//
	// The resolver joins facts to source_authority_maps, and an inner join
	// dropped these silently — so a source nobody had ranked yet was not
	// merely outranked, it was invisible, and its disagreement with the winner
	// never surfaced. "No rule exists for this source" and "this source lost"
	// are different answers, and only one of them means the register is
	// complete. Reported alongside the resolution rather than as an error,
	// because a resolution over the ranked sources is still the correct answer
	// — it is just not the whole picture, and the caller has to be told.
	UnmappedSources []string `json:"unmapped_sources,omitempty"`
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrConflict = errorString("conflict: source_authority_map already exists for this field_family+source_system+effective_from")

	// ErrMapNotFound is returned when no precedence rule carries the given id.
	ErrMapNotFound = errorString("source authority map not found")

	// ErrAlreadySuperseded is returned when a rule that has already been ended
	// is superseded again. Terminal, and not a failure of the request — the
	// window is already closed, which is what the caller wanted.
	ErrAlreadySuperseded = errorString("this precedence rule has already been superseded")

	// ErrSupersedeInPast is returned for an effective_to before now. Ending a
	// rule in the past rewrites which rule was in force at the moment a past
	// resolution was made, so a resolution recorded last week would no longer
	// be explainable by the register.
	ErrSupersedeInPast = errorString("effective_to must not be in the past: a precedence rule cannot be un-applied to decisions already made under it")

	// ErrSupersedeBeforeStart is returned when effective_to precedes the rule's
	// own effective_from, which would leave a window of negative length.
	ErrSupersedeBeforeStart = errorString("effective_to must be after the rule's effective_from")

	// ErrUnknownAuthorityClass is returned for an authority_class outside the
	// §K1 vocabulary. Silently storing a misspelling puts a fact in a class no
	// consumer knows how to weigh, on a table whose entire purpose is to record
	// exactly what a source said and how much it counts.
	ErrUnknownAuthorityClass = errorString("authority_class must be one of AUTHORITATIVE, DERIVED, CACHED")

	// ErrInvalidPaging is returned for an out-of-range limit or offset.
	ErrInvalidPaging = errorString("limit must be between 1 and 500 and offset must not be negative")

	// ErrTenantMissing is returned when a request that touches normalized facts
	// carries no X-Tenant-Id. Distinct from ErrIdentityMissing so a forgotten
	// tenant header is not reported as a missing principal.
	//
	// Load-bearing on the read path specifically: the envelope middleware
	// defaults to write-strict, which admits reads without an envelope, so
	// nothing upstream of this guarantees a tenant on GET /resolve.
	ErrTenantMissing = errorString("tenant context missing")

	// ErrIdentityMissing is returned when a request carries no resolved
	// identity (no X-Principal-Id header) — it never passed gateway-auth-svc's
	// ForwardAuth verification. Fail closed, same as every other service here.
	ErrIdentityMissing = errorString("caller identity missing")
)
