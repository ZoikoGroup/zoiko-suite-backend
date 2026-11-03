package domain

// ORG-07 Party / Counterparty and MDM-01 identity creation (ZS-SVC-I-001 s4.7, ZS-SVC-U-001 s9, s10).
//
// A party is a stable master identity. What is known about it is effective-dated, sourced facts; who it is
// recognised by is external identifiers; what it does for the tenant is explicit, time-aware roles. A role never
// redefines identity, and a fact is never silently overwritten: a proposal that contradicts a verified fact is held
// beside it and a stewardship case is opened.
//
// Everything in this file is pure, so the matching ladder (MDM-05 s18.1) is testable without a database.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// PartyKind is MDM-01 s9.2.
type PartyKind string

const (
	PartyLegalEntity          PartyKind = "LEGAL_ENTITY"
	PartyOrganizationNonLegal PartyKind = "ORGANIZATION_NON_LEGAL"
	PartyNaturalPerson        PartyKind = "NATURAL_PERSON"
	PartyPublicAuthority      PartyKind = "PUBLIC_AUTHORITY"
)

// Valid reports whether k is one of the four kinds.
func (k PartyKind) Valid() bool {
	switch k {
	case PartyLegalEntity, PartyOrganizationNonLegal, PartyNaturalPerson, PartyPublicAuthority:
		return true
	}
	return false
}

// PartyStatus is the s4.7 lifecycle: Candidate -> Active -> Inactive. A merge creates lineage, not a status.
type PartyStatus string

const (
	PartyCandidate PartyStatus = "CANDIDATE"
	PartyActive    PartyStatus = "ACTIVE"
	PartyInactive  PartyStatus = "INACTIVE"
)

// FactType is what a party fact says.
type FactType string

const (
	FactLegalName       FactType = "LEGAL_NAME"
	FactTradingName     FactType = "TRADING_NAME"
	FactFormerName      FactType = "FORMER_NAME"
	FactLocalScriptName FactType = "LOCAL_SCRIPT_NAME"
	FactDisplayName     FactType = "DISPLAY_NAME"
	FactCountry         FactType = "COUNTRY"
	FactAuthorityType   FactType = "AUTHORITY_TYPE"
)

// Valid reports whether t is a known fact type.
func (t FactType) Valid() bool {
	return t.IsName() || t == FactCountry || t == FactAuthorityType
}

// IsName reports whether the fact is one of MDM-01 s10.2's five name types.
func (t FactType) IsName() bool {
	switch t {
	case FactLegalName, FactTradingName, FactFormerName, FactLocalScriptName, FactDisplayName:
		return true
	}
	return false
}

// SingleValued facts have one value in force at a time; names other than the legal name can co-exist.
func (t FactType) SingleValued() bool {
	return t == FactLegalName || t == FactCountry || t == FactAuthorityType
}

// FactStatus is the verification state of a fact.
type FactStatus string

const (
	FactProposed   FactStatus = "PROPOSED"
	FactVerified   FactStatus = "VERIFIED"
	FactSuperseded FactStatus = "SUPERSEDED"
	FactRejected   FactStatus = "REJECTED"
)

// SourceClass says where a fact came from. Authority classes verify a fact on arrival; anything else is held
// PROPOSED until a steward accepts it.
type SourceClass string

const (
	SourceAuthorityRegistry SourceClass = "AUTHORITY_REGISTRY"
	SourceAuthorityDocument SourceClass = "AUTHORITY_DOCUMENT"
	SourceTenantAsserted    SourceClass = "TENANT_ASSERTED"
	SourceSystemImport      SourceClass = "SYSTEM_IMPORT"
	SourceExternalProvider  SourceClass = "EXTERNAL_PROVIDER"
)

// Valid reports whether c is a known source class.
func (c SourceClass) Valid() bool {
	switch c {
	case SourceAuthorityRegistry, SourceAuthorityDocument, SourceTenantAsserted, SourceSystemImport, SourceExternalProvider:
		return true
	}
	return false
}

// Authoritative reports whether a fact from this source verifies on arrival.
func (c SourceClass) Authoritative() bool {
	return c == SourceAuthorityRegistry || c == SourceAuthorityDocument
}

// IdentifierScheme names the system an external identifier belongs to.
type IdentifierScheme string

const (
	SchemeCompanyRegistration IdentifierScheme = "COMPANY_REGISTRATION"
	SchemeTaxID               IdentifierScheme = "TAX_ID"
	SchemeVAT                 IdentifierScheme = "VAT"
	SchemeLEI                 IdentifierScheme = "LEI"
	SchemeDUNS                IdentifierScheme = "DUNS"
	SchemeNationalID          IdentifierScheme = "NATIONAL_ID"
	// SchemeSourceSystem is the ingest-dedup key: scope is the source system, value its identifier for the party.
	SchemeSourceSystem IdentifierScheme = "SOURCE_SYSTEM"
)

// Valid reports whether s is a known scheme.
func (s IdentifierScheme) Valid() bool {
	switch s {
	case SchemeCompanyRegistration, SchemeTaxID, SchemeVAT, SchemeLEI, SchemeDUNS, SchemeNationalID, SchemeSourceSystem:
		return true
	}
	return false
}

// Sensitive schemes are masked in every read unless the caller holds the reveal permission (s4.7: sensitive
// identifiers masked by field policy).
func (s IdentifierScheme) Sensitive() bool { return s == SchemeTaxID || s == SchemeNationalID }

// MaskIdentifier keeps the last four characters of a sensitive identifier and hides the rest. A short value is
// hidden entirely: four visible characters of a five-character identifier is not masking.
func MaskIdentifier(scheme IdentifierScheme, value string) string {
	if !scheme.Sensitive() {
		return value
	}
	r := []rune(value)
	if len(r) <= 6 {
		return strings.Repeat("*", len(r))
	}
	return strings.Repeat("*", len(r)-4) + string(r[len(r)-4:])
}

// RoleType is what a party is to the tenant.
type RoleType string

const (
	RoleCustomer      RoleType = "CUSTOMER"
	RoleSupplier      RoleType = "SUPPLIER"
	RoleCounterparty  RoleType = "COUNTERPARTY"
	RoleSignatory     RoleType = "SIGNATORY"
	RoleBank          RoleType = "BANK"
	RoleRegulator     RoleType = "REGULATOR"
	RoleLandlord      RoleType = "LANDLORD"
	RoleEmployerLink  RoleType = "EMPLOYER_LINK"
	RoleOtherGoverned RoleType = "OTHER_GOVERNED"
)

// Valid reports whether r is a known role type.
func (r RoleType) Valid() bool {
	switch r {
	case RoleCustomer, RoleSupplier, RoleCounterparty, RoleSignatory, RoleBank, RoleRegulator, RoleLandlord,
		RoleEmployerLink, RoleOtherGoverned:
		return true
	}
	return false
}

// Outcomes of CreateParty.
type CreateOutcome string

const (
	OutcomeCreated         CreateOutcome = "CREATED"
	OutcomeMatchedExisting CreateOutcome = "MATCHED_EXISTING"
	OutcomeReviewRequired  CreateOutcome = "REVIEW_REQUIRED"
)

// Resolution case types and states.
const (
	CaseDuplicateReview    = "DUPLICATE_REVIEW"
	CaseIdentifierConflict = "IDENTIFIER_CONFLICT"
	CaseFactConflict       = "FACT_CONFLICT"

	CaseOpen = "OPEN"
)

// ── Requests ─────────────────────────────────────────────────────────────────

// PartyNameInput is one name of a new party.
type PartyNameInput struct {
	Type     FactType `json:"type"`
	Value    string   `json:"value"`
	Language string   `json:"language,omitempty"`
	Script   string   `json:"script,omitempty"`
}

// PartyIdentifierInput is one external identifier of a party.
type PartyIdentifierInput struct {
	Scheme IdentifierScheme `json:"scheme"`
	// Scope is the issuing jurisdiction (an ISO 3166 country code) or, for SOURCE_SYSTEM, the system.
	Scope       string      `json:"scope,omitempty"`
	Value       string      `json:"value"`
	SourceClass SourceClass `json:"source_class,omitempty"`
	EvidenceRef string      `json:"evidence_ref,omitempty"`
}

// CreatePartyRequest is POST /v1/parties.
type CreatePartyRequest struct {
	CreateRequestID string                 `json:"create_request_id"`
	PartyKind       PartyKind              `json:"party_kind"`
	Names           []PartyNameInput       `json:"names"`
	Country         string                 `json:"country,omitempty"`
	Identifiers     []PartyIdentifierInput `json:"identifiers,omitempty"`
	SourceSystem    string                 `json:"source_system,omitempty"`
	SourceRef       string                 `json:"source_ref,omitempty"`
	SourceClass     SourceClass            `json:"source_class"`
	EvidenceRef     string                 `json:"evidence_ref"`
	// ValidFrom (YYYY-MM-DD) is when the facts began to hold.
	ValidFrom string `json:"valid_from"`
	// LegalEntityID links a LEGAL_ENTITY party to the ORG-03 legal entity it is. Legal entities stay owned by ORG-03.
	LegalEntityID string `json:"legal_entity_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// MaxIdentifiers and MaxNames bound a create request.
const (
	MaxPartyIdentifiers = 20
	MaxPartyNames       = 20
)

// Validate checks the request without touching state.
func (r *CreatePartyRequest) Validate() error {
	if strings.TrimSpace(r.CreateRequestID) == "" {
		return fmt.Errorf("create_request_id is required")
	}
	if len(r.CreateRequestID) > 128 {
		return fmt.Errorf("create_request_id is at most 128 characters")
	}
	if !r.PartyKind.Valid() {
		return fmt.Errorf("party_kind must be LEGAL_ENTITY, ORGANIZATION_NON_LEGAL, NATURAL_PERSON or PUBLIC_AUTHORITY")
	}
	if !r.SourceClass.Valid() {
		return fmt.Errorf("source_class is required: AUTHORITY_REGISTRY, AUTHORITY_DOCUMENT, TENANT_ASSERTED, SYSTEM_IMPORT or EXTERNAL_PROVIDER")
	}
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required: a party is recorded with the source it came from")
	}
	if _, err := ParseDate(r.ValidFrom); err != nil {
		return fmt.Errorf("valid_from: %w", err)
	}
	if len(r.Names) == 0 || len(r.Names) > MaxPartyNames {
		return fmt.Errorf("names must hold between 1 and %d names", MaxPartyNames)
	}
	hasLegal := false
	for i, n := range r.Names {
		if !n.Type.IsName() {
			return fmt.Errorf("names[%d].type must be LEGAL_NAME, TRADING_NAME, FORMER_NAME, LOCAL_SCRIPT_NAME or DISPLAY_NAME", i)
		}
		if strings.TrimSpace(n.Value) == "" {
			return fmt.Errorf("names[%d].value is required", i)
		}
		if n.Type == FactLocalScriptName && (strings.TrimSpace(n.Language) == "" || strings.TrimSpace(n.Script) == "") {
			// MDM-01 s10.2: stored with language/script metadata.
			return fmt.Errorf("names[%d]: a LOCAL_SCRIPT_NAME carries its language and script", i)
		}
		if n.Type == FactLegalName {
			if hasLegal {
				return fmt.Errorf("names: a party has one legal name at a time")
			}
			hasLegal = true
		}
	}
	if r.PartyKind == PartyLegalEntity && !hasLegal {
		return fmt.Errorf("a LEGAL_ENTITY party needs a LEGAL_NAME: a trading or display name never substitutes for it")
	}
	if r.Country != "" && !validCountry(r.Country) {
		return fmt.Errorf("country must be an ISO 3166-1 alpha-2 code")
	}
	if r.PartyKind == PartyPublicAuthority && r.Country == "" {
		return fmt.Errorf("a PUBLIC_AUTHORITY needs the country (jurisdiction) it acts for")
	}
	if r.LegalEntityID != "" && r.PartyKind != PartyLegalEntity {
		return fmt.Errorf("only a LEGAL_ENTITY party links to a legal entity")
	}
	if len(r.Identifiers) > MaxPartyIdentifiers {
		return fmt.Errorf("identifiers holds at most %d", MaxPartyIdentifiers)
	}
	for i := range r.Identifiers {
		if err := r.Identifiers[i].Validate(); err != nil {
			return fmt.Errorf("identifiers[%d]: %w", i, err)
		}
	}
	if (r.SourceSystem == "") != (r.SourceRef == "") {
		return fmt.Errorf("source_system and source_ref go together")
	}
	return nil
}

// Validate checks one identifier.
func (i *PartyIdentifierInput) Validate() error {
	if !i.Scheme.Valid() {
		return fmt.Errorf("scheme is not a known identifier scheme")
	}
	if strings.TrimSpace(i.Value) == "" || NormalizeIdentifier(i.Value) == "" {
		return fmt.Errorf("value is required")
	}
	if i.Scheme == SchemeSourceSystem && strings.TrimSpace(i.Scope) == "" {
		return fmt.Errorf("a SOURCE_SYSTEM identifier names its system in scope")
	}
	if i.Scheme != SchemeSourceSystem && i.Scope != "" && !validCountry(i.Scope) {
		return fmt.Errorf("scope must be an ISO 3166-1 alpha-2 code")
	}
	if i.SourceClass != "" && !i.SourceClass.Valid() {
		return fmt.Errorf("source_class is not a known source class")
	}
	return nil
}

func validCountry(c string) bool {
	if len(c) != 2 {
		return false
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// ParseDate reads YYYY-MM-DD.
func ParseDate(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("must be a date, YYYY-MM-DD")
	}
	return t.UTC(), nil
}

// Fingerprint identifies what a create request asks for, so a replayed create_request_id carrying a different
// request is refused rather than answered with the first one's result. Correlation id and request id are not
// part of it.
func (r *CreatePartyRequest) Fingerprint() string {
	type id struct{ S, C, V string }
	type nm struct{ T, V, L, S string }
	var ids []id
	for _, i := range r.Identifiers {
		ids = append(ids, id{string(i.Scheme), NormalizeScope(i.Scheme, i.Scope), NormalizeIdentifier(i.Value)})
	}
	sort.Slice(ids, func(a, b int) bool { return fmt.Sprint(ids[a]) < fmt.Sprint(ids[b]) })
	var names []nm
	for _, n := range r.Names {
		names = append(names, nm{string(n.Type), strings.TrimSpace(n.Value), n.Language, n.Script})
	}
	sort.Slice(names, func(a, b int) bool { return fmt.Sprint(names[a]) < fmt.Sprint(names[b]) })
	raw, _ := json.Marshal(struct {
		K  string
		N  []nm
		C  string
		I  []id
		SS string
		SR string
		SC string
		VF string
		LE string
	}{string(r.PartyKind), names, r.Country, ids, r.SourceSystem, r.SourceRef, string(r.SourceClass), r.ValidFrom, r.LegalEntityID})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ProposeFactRequest is POST /v1/parties/{id}/facts:propose.
type ProposeFactRequest struct {
	FactType FactType `json:"fact_type"`
	Value    string   `json:"value"`
	Language string   `json:"language,omitempty"`
	Script   string   `json:"script,omitempty"`
	// ValidFrom is required: a fact says from when it holds.
	ValidFrom             string      `json:"valid_from"`
	ValidTo               string      `json:"valid_to,omitempty"`
	SourceClass           SourceClass `json:"source_class"`
	EvidenceRef           string      `json:"evidence_ref"`
	ExpectedMasterVersion int64       `json:"expected_master_version"`
	CorrelationID         string      `json:"correlation_id,omitempty"`
}

// Validate checks the request without touching state.
func (r *ProposeFactRequest) Validate() error {
	if !r.FactType.Valid() {
		return fmt.Errorf("fact_type is not a known party fact type")
	}
	if strings.TrimSpace(r.Value) == "" {
		return fmt.Errorf("value is required")
	}
	if r.FactType == FactCountry && !validCountry(r.Value) {
		return fmt.Errorf("a COUNTRY fact is an ISO 3166-1 alpha-2 code")
	}
	if r.FactType == FactLocalScriptName && (strings.TrimSpace(r.Language) == "" || strings.TrimSpace(r.Script) == "") {
		return fmt.Errorf("a LOCAL_SCRIPT_NAME carries its language and script")
	}
	from, err := ParseDate(r.ValidFrom)
	if err != nil {
		return fmt.Errorf("valid_from: %w", err)
	}
	if r.ValidTo != "" {
		to, err := ParseDate(r.ValidTo)
		if err != nil {
			return fmt.Errorf("valid_to: %w", err)
		}
		if !to.After(from) {
			return fmt.Errorf("valid_to must be after valid_from")
		}
	}
	if !r.SourceClass.Valid() {
		return fmt.Errorf("source_class is required")
	}
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required: a change to a party is recorded with its source")
	}
	return nil
}

// AddPartyRoleRequest is POST /v1/parties/{id}/roles.
type AddPartyRoleRequest struct {
	RoleType              RoleType `json:"role_type"`
	ValidFrom             string   `json:"valid_from"`
	EvidenceRef           string   `json:"evidence_ref"`
	ExpectedMasterVersion int64    `json:"expected_master_version"`
	CorrelationID         string   `json:"correlation_id,omitempty"`
}

// Validate checks the request without touching state.
func (r *AddPartyRoleRequest) Validate() error {
	if !r.RoleType.Valid() {
		return fmt.Errorf("role_type is not a known party role")
	}
	if _, err := ParseDate(r.ValidFrom); err != nil {
		return fmt.Errorf("valid_from: %w", err)
	}
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required: a role is explicit and evidenced")
	}
	return nil
}

// RemovePartyRoleRequest is POST /v1/parties/{id}/roles/{roleID}/removal.
type RemovePartyRoleRequest struct {
	EndDate               string `json:"end_date"`
	Reason                string `json:"reason"`
	ExpectedMasterVersion int64  `json:"expected_master_version"`
	CorrelationID         string `json:"correlation_id,omitempty"`
}

// Validate checks the request without touching state.
func (r *RemovePartyRoleRequest) Validate() error {
	if _, err := ParseDate(r.EndDate); err != nil {
		return fmt.Errorf("end_date: %w", err)
	}
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}

// LinkIdentifierRequest is POST /v1/parties/{id}/identifiers.
type LinkIdentifierRequest struct {
	PartyIdentifierInput
	ExpectedMasterVersion int64  `json:"expected_master_version"`
	CorrelationID         string `json:"correlation_id,omitempty"`
}

// Validate checks the request without touching state.
func (r *LinkIdentifierRequest) Validate() error {
	if err := r.PartyIdentifierInput.Validate(); err != nil {
		return err
	}
	if !r.SourceClass.Valid() {
		return fmt.Errorf("source_class is required")
	}
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required: an identifier is linked with its source")
	}
	return nil
}

// PartySearchQuery is GET /v1/parties.
type PartySearchQuery struct {
	Text   string
	Kind   PartyKind
	Role   RoleType
	Status PartyStatus
	Limit  int
	After  string
}

// ── Views ────────────────────────────────────────────────────────────────────

// Party is the master record.
type Party struct {
	PartyID       string      `json:"party_id"`
	TenantID      string      `json:"tenant_id"`
	PartyKind     PartyKind   `json:"party_kind"`
	Status        PartyStatus `json:"status"`
	LegalEntityID *string     `json:"legal_entity_id,omitempty"`
	MasterVersion int64       `json:"master_version"`
	CreatedBy     string      `json:"created_by"`
	CreatedAt     time.Time   `json:"created_at"`
}

// PartyFact is one effective-dated, sourced fact.
type PartyFact struct {
	FactID      string      `json:"fact_id"`
	FactType    FactType    `json:"fact_type"`
	Value       string      `json:"value"`
	Language    string      `json:"language,omitempty"`
	Script      string      `json:"script,omitempty"`
	ValidFrom   string      `json:"valid_from"`
	ValidTo     string      `json:"valid_to,omitempty"`
	RecordedAt  time.Time   `json:"recorded_at"`
	Status      FactStatus  `json:"status"`
	SourceClass SourceClass `json:"source_class"`
	EvidenceRef string      `json:"evidence_ref"`
	ProposedBy  string      `json:"proposed_by"`
}

// PartyIdentifier is an external identifier, masked unless the caller may reveal it.
type PartyIdentifier struct {
	IdentifierID       string           `json:"identifier_id"`
	PartyID            string           `json:"party_id,omitempty"`
	Scheme             IdentifierScheme `json:"scheme"`
	Scope              string           `json:"scope,omitempty"`
	Value              string           `json:"value"`
	Masked             bool             `json:"masked"`
	VerificationStatus string           `json:"verification_status"`
	EvidenceRef        string           `json:"evidence_ref"`
	LinkedAt           time.Time        `json:"linked_at"`
}

// PartyRole is a role a party plays for the tenant.
type PartyRole struct {
	RoleID      string    `json:"role_id"`
	RoleType    RoleType  `json:"role_type"`
	ValidFrom   string    `json:"valid_from"`
	ValidTo     string    `json:"valid_to,omitempty"`
	EvidenceRef string    `json:"evidence_ref"`
	AddedBy     string    `json:"added_by"`
	AddedAt     time.Time `json:"added_at"`
	RemovedBy   string    `json:"removed_by,omitempty"`
	RemoveNote  string    `json:"remove_reason,omitempty"`
}

// PartyView is the effective-dated master view (GetPartyAsOf): what was in force at AsOf as ZoikoSuite knew it at
// KnownAt, with quality and version metadata.
type PartyView struct {
	Party        Party             `json:"party"`
	AsOf         string            `json:"as_of"`
	KnownAt      time.Time         `json:"known_at"`
	Names        []PartyFact       `json:"names"`
	Facts        []PartyFact       `json:"facts"`
	PendingFacts []PartyFact       `json:"pending_facts"`
	Identifiers  []PartyIdentifier `json:"identifiers"`
	Roles        []PartyRole       `json:"roles"`
	QualityFlags []string          `json:"quality_flags"`
}

// PartySummary is a search or find result.
type PartySummary struct {
	PartyID   string      `json:"party_id"`
	PartyKind PartyKind   `json:"party_kind"`
	Status    PartyStatus `json:"status"`
	LegalName string      `json:"legal_name,omitempty"`
}

// MatchCandidate is one existing party a request resembles, with why.
type MatchCandidate struct {
	PartyID string    `json:"party_id"`
	Tier    MatchTier `json:"tier"`
	Score   float64   `json:"score"`
	Basis   []string  `json:"basis"`
}

// ResolutionCase is a stewardship case.
type ResolutionCase struct {
	CaseID          string           `json:"case_id"`
	TenantID        string           `json:"tenant_id"`
	CaseType        string           `json:"case_type"`
	Status          string           `json:"status"`
	SubjectPartyID  *string          `json:"subject_party_id,omitempty"`
	Candidates      []MatchCandidate `json:"candidates"`
	CreateRequestID string           `json:"create_request_id,omitempty"`
	Reason          string           `json:"reason"`
	OpenedBy        string           `json:"opened_by"`
	OpenedAt        time.Time        `json:"opened_at"`
}

// CreatePartyResult is the answer to CreateParty.
type CreatePartyResult struct {
	Outcome    CreateOutcome    `json:"outcome"`
	Party      *Party           `json:"party,omitempty"`
	Match      *MatchCandidate  `json:"match,omitempty"`
	Case       *ResolutionCase  `json:"case,omitempty"`
	Candidates []MatchCandidate `json:"candidates,omitempty"`
	Replayed   bool             `json:"replayed"`
}

// ── Matching (MDM-05 s18.1) ──────────────────────────────────────────────────

// MatchTier is the MDM-05 evidence ladder.
type MatchTier string

const (
	// TierA: a verified identifier in the right scheme and scope maps to one active party. May link automatically.
	TierA MatchTier = "A"
	// TierB: a strong composite: the same normalised name and the same country.
	TierB MatchTier = "B"
	// TierC: probabilistic: name similarity. Always REVIEW_REQUIRED.
	TierC MatchTier = "C"
	// TierD: conflict: identifiers map to different parties, or the kind disagrees. Never automatic.
	TierD MatchTier = "D"
)

// SimilarityThreshold is the bigram Dice score at which two names are candidates for review.
const SimilarityThreshold = 0.85

// legalSuffixes are dropped from a name for comparison only; the source value is kept untouched.
var legalSuffixes = map[string]bool{
	"ltd": true, "limited": true, "inc": true, "incorporated": true, "llc": true, "plc": true, "gmbh": true,
	"sa": true, "srl": true, "bv": true, "nv": true, "ag": true, "pty": true, "co": true, "corp": true,
	"corporation": true, "company": true, "llp": true, "lp": true, "sarl": true, "spa": true, "oy": true,
}

// NormalizeName is the comparison form of a name: lower case, letters and digits only, one space between words,
// legal-form suffixes dropped. It never replaces the source value.
func NormalizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r == '.':
			// "S.A." is "sa", not two words.
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(foldRune(r))
		default:
			b.WriteRune(' ')
		}
	}
	words := strings.Fields(b.String())
	kept := words[:0:0]
	for _, w := range words {
		if !legalSuffixes[w] {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		// A name that is only a legal form ("Limited") is compared as itself.
		kept = words
	}
	return strings.Join(kept, " ")
}

// foldRune maps common accented Latin letters to their base letter so "Müller" and "Muller" compare equal.
func foldRune(r rune) rune {
	const from = "àáâãäåçèéêëìíîïñòóôõöùúûüýÿ"
	const to = "aaaaaaceeeeiiiinooooouuuuyy"
	for i, f := range []rune(from) {
		if f == r {
			return []rune(to)[i]
		}
	}
	return r
}

// NameTokens are the words of a normalised name, for candidate lookup.
func NameTokens(normalized string) []string { return strings.Fields(normalized) }

// NormalizeIdentifier is the comparison form of an identifier: upper case, letters and digits only.
func NormalizeIdentifier(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NameSimilarity is the Dice coefficient over character bigrams of two normalised names, 0 to 1.
func NameSimilarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	ba, bb := bigrams(a), bigrams(b)
	if len(ba) == 0 || len(bb) == 0 {
		return 0
	}
	counts := map[string]int{}
	for _, g := range ba {
		counts[g]++
	}
	hit := 0
	for _, g := range bb {
		if counts[g] > 0 {
			counts[g]--
			hit++
		}
	}
	return 2 * float64(hit) / float64(len(ba)+len(bb))
}

func bigrams(s string) []string {
	r := []rune(s)
	if len(r) < 2 {
		return nil
	}
	out := make([]string, 0, len(r)-1)
	for i := 0; i+1 < len(r); i++ {
		out = append(out, string(r[i:i+2]))
	}
	return out
}

// KnownParty is an existing party as the matcher sees it.
type KnownParty struct {
	PartyID string
	Kind    PartyKind
	Country string
	// Names are normalised legal, trading and former names.
	Names       []string
	Identifiers []KnownIdentifier
}

// KnownIdentifier is an existing ACTIVE identifier.
type KnownIdentifier struct {
	Scheme   IdentifierScheme
	Scope    string
	Value    string // normalised
	Verified bool
}

// MatchRequest is what a create request resembles, normalised.
type MatchRequest struct {
	Kind        PartyKind
	Country     string
	Names       []string // normalised, deduplicated
	Identifiers []KnownIdentifier
}

// MatchDecision is the matcher's verdict.
type MatchDecision struct {
	// SafeLink is set when exactly one party is a Tier A match: the caller returns it instead of creating.
	SafeLink *MatchCandidate
	// Review is set when the request is ambiguous or conflicts: the caller opens a case, creates nothing.
	Review     bool
	CaseType   string
	Candidates []MatchCandidate
}

// ClassifyMatch applies the MDM-05 ladder. It never decides to merge: the strongest it permits is returning an
// existing party for a Tier A identifier match. Anything uncertain is REVIEW_REQUIRED (s10.1 step 5), and a
// conflict (Tier D) can never be linked.
func ClassifyMatch(req MatchRequest, known []KnownParty) MatchDecision {
	byParty := map[string]*MatchCandidate{}
	add := func(id string, tier MatchTier, score float64, basis string) {
		c := byParty[id]
		if c == nil {
			c = &MatchCandidate{PartyID: id, Tier: tier, Score: score}
			byParty[id] = c
		}
		if rank(tier) > rank(c.Tier) {
			c.Tier = tier
		}
		if score > c.Score {
			c.Score = score
		}
		c.Basis = append(c.Basis, basis)
	}

	kinds := map[string]PartyKind{}
	idParties := map[string]bool{}
	for _, k := range known {
		kinds[k.PartyID] = k.Kind
		for _, ki := range k.Identifiers {
			for _, ri := range req.Identifiers {
				if ki.Scheme != ri.Scheme || ki.Scope != ri.Scope || ki.Value != ri.Value {
					continue
				}
				idParties[k.PartyID] = true
				if ki.Verified || ki.Scheme == SchemeSourceSystem {
					add(k.PartyID, TierA, 1, fmt.Sprintf("identifier %s %s matches a verified identifier", ri.Scheme, ri.Scope))
				} else {
					// An identifier the tenant has only asserted is evidence, not proof.
					add(k.PartyID, TierB, 0.95, fmt.Sprintf("identifier %s %s matches an unverified identifier", ri.Scheme, ri.Scope))
				}
			}
		}
	}

	if len(idParties) > 0 {
		ids := sortedKeys(idParties)
		if len(ids) > 1 {
			// The request's identifiers point at different parties: the sources disagree. Tier D.
			for _, id := range ids {
				byParty[id].Tier = TierD
				byParty[id].Basis = append(byParty[id].Basis, "the request's identifiers map to different parties")
			}
			return MatchDecision{Review: true, CaseType: CaseIdentifierConflict, Candidates: orderedCandidates(byParty)}
		}
		id := ids[0]
		if kinds[id] != req.Kind {
			byParty[id].Tier = TierD
			byParty[id].Basis = append(byParty[id].Basis, fmt.Sprintf("the identifier belongs to a %s party, the request is a %s", kinds[id], req.Kind))
			return MatchDecision{Review: true, CaseType: CaseIdentifierConflict, Candidates: orderedCandidates(byParty)}
		}
		if byParty[id].Tier == TierA {
			c := *byParty[id]
			return MatchDecision{SafeLink: &c, Candidates: []MatchCandidate{c}}
		}
		// unverified identifier: fall through to review with this candidate present
	}

	for _, k := range known {
		if k.Kind != req.Kind {
			continue
		}
		best := 0.0
		for _, rn := range req.Names {
			for _, kn := range k.Names {
				if s := NameSimilarity(rn, kn); s > best {
					best = s
				}
			}
		}
		switch {
		case best == 1 && req.Country != "" && req.Country == k.Country:
			add(k.PartyID, TierB, 1, "same normalised name and country")
		case best == 1:
			add(k.PartyID, TierC, 1, "same normalised name, country not the same or unknown")
		case best >= SimilarityThreshold:
			add(k.PartyID, TierC, best, fmt.Sprintf("name similarity %.2f", best))
		}
	}
	if len(byParty) == 0 {
		return MatchDecision{}
	}
	return MatchDecision{Review: true, CaseType: CaseDuplicateReview, Candidates: orderedCandidates(byParty)}
}

func rank(t MatchTier) int {
	switch t {
	case TierA:
		return 4
	case TierD:
		return 5
	case TierB:
		return 3
	case TierC:
		return 2
	}
	return 0
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orderedCandidates(m map[string]*MatchCandidate) []MatchCandidate {
	out := make([]MatchCandidate, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].PartyID < out[j].PartyID
	})
	return out
}

// MatchRequest builds the matcher's view of a create request: names normalised for comparison, identifiers in
// their comparison form, the source-system reference included as the scheme-proven identifier it is.
func (r *CreatePartyRequest) MatchRequest() MatchRequest {
	m := MatchRequest{Kind: r.PartyKind, Country: strings.ToUpper(strings.TrimSpace(r.Country))}
	seen := map[string]bool{}
	for _, n := range r.Names {
		// Display and local-script names are labels, not identity evidence.
		if n.Type != FactLegalName && n.Type != FactTradingName && n.Type != FactFormerName {
			continue
		}
		if norm := NormalizeName(n.Value); norm != "" && !seen[norm] {
			seen[norm] = true
			m.Names = append(m.Names, norm)
		}
	}
	for _, i := range r.AllIdentifiers() {
		m.Identifiers = append(m.Identifiers, KnownIdentifier{
			Scheme: i.Scheme, Scope: NormalizeScope(i.Scheme, i.Scope), Value: NormalizeIdentifier(i.Value),
		})
	}
	return m
}

// AllIdentifiers is the request's identifiers plus its source-system reference, if any.
func (r *CreatePartyRequest) AllIdentifiers() []PartyIdentifierInput {
	out := append([]PartyIdentifierInput(nil), r.Identifiers...)
	if r.SourceSystem != "" {
		out = append(out, PartyIdentifierInput{Scheme: SchemeSourceSystem, Scope: r.SourceSystem, Value: r.SourceRef,
			SourceClass: SourceSystemImport, EvidenceRef: r.EvidenceRef})
	}
	return out
}

// NormalizeScope is the comparison form of an identifier scope: a jurisdiction is upper case; a source system is
// the name its owner gave it, trimmed.
func NormalizeScope(scheme IdentifierScheme, scope string) string {
	scope = strings.TrimSpace(scope)
	if scheme == SchemeSourceSystem {
		return scope
	}
	return strings.ToUpper(scope)
}
