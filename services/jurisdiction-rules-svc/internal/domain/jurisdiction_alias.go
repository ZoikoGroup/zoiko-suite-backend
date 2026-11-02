package domain

// REF-01 aliases and code resolution (ZS-SVC-I-001 s4.11):
// AmendJurisdictionAlias, ResolveJurisdiction, MapExternalJurisdictionCode.
//
// A jurisdiction has one stable id and many CODES other systems know it by. The
// failure rule is the heart of it: "Unknown/retired codes fail explicit
// resolution where legal treatment depends on them; never guess from free
// text." Resolving a code therefore has four distinct refusals (unknown, retired, not
// mapped at that time, ambiguous) and never falls back to a near match.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Well-known alias systems. Any other system name is an external system's own
// code list, which this service records and does not interpret.
const (
	SystemISO3166Alpha2  = "ISO3166-1-ALPHA2"
	SystemISO3166Alpha3  = "ISO3166-1-ALPHA3"
	SystemISO3166Numeric = "ISO3166-1-NUMERIC"
	SystemISO3166Sub     = "ISO3166-2"
	SystemUNM49          = "UN-M49"

	// SystemInternal is not an alias system: it names the jurisdiction's own
	// jurisdiction_code. It is reserved so no alias can masquerade as it.
	SystemInternal = "INTERNAL"
)

// JurisdictionAlias is one mapping of a (system, code) to a jurisdiction, in
// force over [ValidFrom, ValidTo).
type JurisdictionAlias struct {
	AliasID          string     `json:"alias_id"`
	JurisdictionID   string     `json:"jurisdiction_id"`
	AliasSystem      string     `json:"alias_system"`
	AliasCode        string     `json:"alias_code"`
	SourceStandard   string     `json:"source_standard"`
	SourceVersion    string     `json:"source_version"`
	MappingRationale string     `json:"mapping_rationale"`
	EvidenceRef      *string    `json:"evidence_ref,omitempty"`
	ValidFrom        time.Time  `json:"valid_from"`
	ValidTo          *time.Time `json:"valid_to"`
	CreatedBy        string     `json:"created_by"`
	CreatedAt        time.Time  `json:"created_at"`
	EndedBy          *string    `json:"ended_by,omitempty"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	EndReason        *string    `json:"end_reason,omitempty"`
}

// AddAliasParams is the AmendJurisdictionAlias command.
type AddAliasParams struct {
	JurisdictionID   string
	AliasSystem      string
	AliasCode        string
	SourceStandard   string
	SourceVersion    string
	MappingRationale string
	EvidenceRef      string
	ValidFrom        time.Time
	ValidTo          *time.Time
	// ReplacesAliasID, when set, ends that mapping of the same (system, code) at
	// ValidFrom in the same transaction: the code now means something else (or
	// the same thing under a new source version).
	ReplacesAliasID string
	ActorID         string
}

// EndAliasParams ends a mapping.
type EndAliasParams struct {
	AliasID        string
	JurisdictionID string
	ValidTo        time.Time
	ActorID        string
	Reason         string
}

// ResolveQuery asks what a code meant at an instant.
type ResolveQuery struct {
	// System is the code system. Empty means the jurisdictions' own
	// jurisdiction_code (the internal code).
	System string
	Code   string
	// Type narrows an internal-code lookup to one jurisdiction_type, which is how
	// an ambiguous code ("GB" is both a country and a tax authority) is made
	// unambiguous by the caller rather than guessed by the service.
	Type string
	AsOf time.Time
}

// CodeResolution is a code resolved to exactly one jurisdiction as it was at AsOf.
type CodeResolution struct {
	JurisdictionID       string  `json:"jurisdiction_id"`
	JurisdictionCode     string  `json:"jurisdiction_code"`
	JurisdictionName     string  `json:"jurisdiction_name"`
	JurisdictionType     string  `json:"jurisdiction_type"`
	AuthorityType        string  `json:"authority_type"`
	ParentJurisdictionID *string `json:"parent_jurisdiction_id"`

	AsOf      time.Time `json:"as_of"`
	VersionNo int       `json:"version_no"`

	// MatchedVia is INTERNAL for a jurisdiction_code match, or the alias system.
	MatchedVia string `json:"matched_via"`
	// Alias is the mapping used, with its source and rationale; nil for an
	// internal-code match.
	Alias *JurisdictionAlias `json:"alias,omitempty"`
}

// ── errors ──────────────────────────────────────────────────────────────────

// ErrUnknownJurisdictionCode: nothing is known by that code in that system. It
// is never answered with a near match.
var ErrUnknownJurisdictionCode = errorString("no jurisdiction is known by that code")

// ErrJurisdictionRetired: the code resolves to a jurisdiction that had been
// retired by the as-of instant.
var ErrJurisdictionRetired = errorString("the jurisdiction was retired")

// ErrAmbiguousJurisdictionCode: more than one jurisdiction answers to the code.
var ErrAmbiguousJurisdictionCode = errorString("more than one jurisdiction is known by that code")

// ErrAliasNotEffective: the code is mapped, but not at the instant asked about.
var ErrAliasNotEffective = errorString("the code was not mapped at that time")

// ErrAliasConflict: the (system, code) already means a different jurisdiction,
// or the same one on different evidence, during part of that interval.
var ErrAliasConflict = errorString("the code is already mapped during part of that interval")

// ErrAliasNotFound: no such mapping on that jurisdiction.
var ErrAliasNotFound = errorString("alias not found")

// ErrInvalidAlias wraps what is wrong with an alias request.
var ErrInvalidAlias = errorString("invalid alias")

func invalidAlias(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidAlias, fmt.Sprintf(format, args...))
}

// RetiredError says which jurisdiction the code resolved to and what became of
// it, so a caller can follow the successor rather than just fail.
type RetiredError struct {
	JurisdictionID string
	Code           string
	RetiredAt      *time.Time
	SupersededBy   *string
	Reason         *string
}

func (e *RetiredError) Error() string {
	when := "at an earlier time"
	if e.RetiredAt != nil {
		when = "on " + e.RetiredAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s (jurisdiction %s) was retired %s", e.Code, e.JurisdictionID, when)
}

func (e *RetiredError) Is(target error) bool { return target == ErrJurisdictionRetired }

// AmbiguityCandidate is one of the jurisdictions a code answers to.
type AmbiguityCandidate struct {
	JurisdictionID   string `json:"jurisdiction_id"`
	JurisdictionType string `json:"jurisdiction_type"`
	JurisdictionName string `json:"jurisdiction_name"`
}

// AmbiguousError lists the candidates. The caller narrows by type; the service
// does not choose.
type AmbiguousError struct {
	Code       string
	Candidates []AmbiguityCandidate
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%q is the code of %d jurisdictions; narrow the lookup by jurisdiction type", e.Code, len(e.Candidates))
}

func (e *AmbiguousError) Is(target error) bool { return target == ErrAmbiguousJurisdictionCode }

// AliasNotEffectiveError says the code is real but was not mapped at AsOf, and
// over what period it ever was.
type AliasNotEffectiveError struct {
	System, Code string
	AsOf         time.Time
	From         time.Time
	To           *time.Time
}

func (e *AliasNotEffectiveError) Error() string {
	if e.AsOf.Before(e.From) {
		return fmt.Sprintf("%s code %q was not yet mapped at %s (first mapped %s)",
			e.System, e.Code, e.AsOf.UTC().Format(time.RFC3339), e.From.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("%s code %q was no longer mapped at %s", e.System, e.Code, e.AsOf.UTC().Format(time.RFC3339))
}

func (e *AliasNotEffectiveError) Is(target error) bool { return target == ErrAliasNotEffective }

// ── validation ──────────────────────────────────────────────────────────────

var (
	systemPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9._:-]{1,63}$`)
	// External codes are a system's own business; keep to a safe, printable set so
	// nothing odd reaches a log or a URL.
	externalCodePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/ -]{0,62}[A-Za-z0-9]$|^[A-Za-z0-9]$`)

	isoFormats = map[string]*regexp.Regexp{
		SystemISO3166Alpha2:  regexp.MustCompile(`^[A-Z]{2}$`),
		SystemISO3166Alpha3:  regexp.MustCompile(`^[A-Z]{3}$`),
		SystemISO3166Numeric: regexp.MustCompile(`^[0-9]{3}$`),
		SystemISO3166Sub:     regexp.MustCompile(`^[A-Z]{2}-[A-Z0-9]{1,3}$`),
		SystemUNM49:          regexp.MustCompile(`^[0-9]{3}$`),
	}
)

// NormalizeCode is the one normalisation applied to a code: surrounding
// whitespace and case. It is deliberately all.
func NormalizeCode(code string) string { return strings.ToUpper(strings.TrimSpace(code)) }

// ValidateCode checks that a code is well formed for its system. For an ISO
// system the format is the standard's; for any other system it is a safe,
// printable code.
func ValidateCode(system, code string) error {
	norm := NormalizeCode(code)
	if norm == "" {
		return invalidAlias("alias_code is required")
	}
	if re, ok := isoFormats[system]; ok {
		if !re.MatchString(norm) {
			return invalidAlias("%q is not a valid %s code", code, system)
		}
		return nil
	}
	if !externalCodePattern.MatchString(strings.TrimSpace(code)) {
		return invalidAlias("alias_code %q has characters outside letters, digits and . _ : / -", code)
	}
	return nil
}

// Validate checks everything that can be decided from the request alone. What
// needs the database (the jurisdiction is in force, no conflicting mapping) is
// the store's.
func (p AddAliasParams) Validate() error {
	if strings.TrimSpace(p.JurisdictionID) == "" {
		return invalidAlias("jurisdiction_id is required")
	}
	sys := strings.TrimSpace(p.AliasSystem)
	switch {
	case sys == "":
		return invalidAlias("alias_system is required")
	case sys == SystemInternal:
		return invalidAlias("alias_system %q is reserved for the jurisdiction's own code", SystemInternal)
	case sys != p.AliasSystem || !systemPattern.MatchString(sys):
		return invalidAlias("alias_system must be upper-case letters, digits and . _ : - (2 to 64 characters)")
	}
	if err := ValidateCode(p.AliasSystem, p.AliasCode); err != nil {
		return err
	}
	if strings.TrimSpace(p.SourceStandard) == "" || strings.TrimSpace(p.SourceVersion) == "" {
		return invalidAlias("source_standard and source_version are required: a mapping without its source cannot be audited")
	}
	if strings.TrimSpace(p.MappingRationale) == "" {
		return invalidAlias("mapping_rationale is required")
	}
	if len(p.SourceStandard) > 128 || len(p.SourceVersion) > 64 {
		return invalidAlias("source_standard is at most 128 characters and source_version at most 64")
	}
	if p.ValidFrom.IsZero() {
		return invalidAlias("valid_from is required")
	}
	if p.ValidTo != nil && !p.ValidTo.After(p.ValidFrom) {
		return invalidAlias("valid_to must be after valid_from")
	}
	return nil
}

// Validate checks an end-alias request.
func (p EndAliasParams) Validate() error {
	if p.ValidTo.IsZero() {
		return invalidAlias("valid_to is required")
	}
	if strings.TrimSpace(p.Reason) == "" {
		return invalidAlias("reason is required: ending a mapping is as material as making one")
	}
	return nil
}

// IsNotFoundLike reports whether err means "nothing here" rather than a fault,
// for callers that fold the not-found family.
func IsNotFoundLike(err error) bool {
	return errors.Is(err, ErrUnknownJurisdictionCode) || errors.Is(err, ErrJurisdictionNotFound) || errors.Is(err, ErrAliasNotFound)
}
