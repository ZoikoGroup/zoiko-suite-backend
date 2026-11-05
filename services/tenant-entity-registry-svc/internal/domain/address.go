package domain

// ORG-08 Address (ZS-SVC-I-001 s4.8, ZS-SVC-U-001 s13, s14.2).
//
// An address is a stable identity with append-only versions. The structured fields are kept exactly as entered; the
// normalized form is a comparison aid and never replaces them. An address, verified or not, proves nothing about
// legal establishment, tax nexus or registered-office status: a REGISTERED address is recorded only from authority
// evidence, and verification only reports what a verifier observed.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// AddressType is the capacity an address is recorded in. Custom types are domain-safe: X_ then upper-case words.
type AddressType string

const (
	AddressRegistered      AddressType = "REGISTERED"
	AddressPrincipalPlace  AddressType = "PRINCIPAL_PLACE"
	AddressOperating       AddressType = "OPERATING"
	AddressBilling         AddressType = "BILLING"
	AddressRemittance      AddressType = "REMITTANCE"
	AddressTax             AddressType = "TAX"
	AddressEmploymentEstab AddressType = "EMPLOYMENT_ESTABLISHMENT"
)

// NeedsAuthority reports whether recording this type is a legal assertion. A registered office is where the law says
// an entity is, not where a postal service can deliver, so only authority evidence can record one (s14.2).
func (t AddressType) NeedsAuthority() bool { return t == AddressRegistered }

// ErrRegisteredNeedsAuthority is returned for a REGISTERED address that cites no authority source.
var ErrRegisteredNeedsAuthority = fmt.Errorf("a REGISTERED address is a legal assertion and needs source_class AUTHORITY_REGISTRY or AUTHORITY_DOCUMENT")

var customAddressType = regexp.MustCompile(`^X_[A-Z0-9_]{1,30}$`)

// Valid reports whether t is a known or domain-safe custom address type.
func (t AddressType) Valid() bool {
	switch t {
	case AddressRegistered, AddressPrincipalPlace, AddressOperating, AddressBilling, AddressRemittance,
		AddressTax, AddressEmploymentEstab:
		return true
	}
	return customAddressType.MatchString(string(t))
}

// AddressState is the verification state of one version.
type AddressState string

const (
	AddressUnverified AddressState = "UNVERIFIED"
	AddressVerified   AddressState = "VERIFIED"
	AddressSuperseded AddressState = "SUPERSEDED"
)

// VerificationResult is what a verifier observed.
type VerificationResult string

const (
	VerifyPassed        VerificationResult = "PASSED"
	VerifyLowConfidence VerificationResult = "LOW_CONFIDENCE"
	VerifyFailed        VerificationResult = "FAILED"
)

// Verifiers an operator may name. Only LOCAL_RULES is available; an external provider is BLOCKED until the
// external data feed service exists (ORG-08, plan ORG-6).
const (
	VerifierLocalRules = "LOCAL_RULES"
	VerifierExternal   = "EXTERNAL_PROVIDER"
)

// AddressVersion is one effective-dated version of an address.
type AddressVersion struct {
	VersionNo         int          `json:"version_no"`
	Lines             []string     `json:"lines"`
	Locality          string       `json:"locality"`
	Subdivision       string       `json:"subdivision,omitempty"`
	PostalCode        string       `json:"postal_code,omitempty"`
	Country           string       `json:"country"`
	Formatted         string       `json:"formatted,omitempty"`
	Normalized        string       `json:"normalized"`
	ValidFrom         string       `json:"valid_from"`
	ValidTo           string       `json:"valid_to,omitempty"`
	VerificationState AddressState `json:"verification_state"`
	SourceClass       SourceClass  `json:"source_class"`
	EvidenceRef       string       `json:"evidence_ref"`
	RecordedBy        string       `json:"recorded_by"`
	RecordedAt        time.Time    `json:"recorded_at"`
	SupersededAt      *time.Time   `json:"superseded_at,omitempty"`
	Reason            string       `json:"reason,omitempty"`
}

// Address is the stable identity.
type Address struct {
	AddressID      string      `json:"address_id"`
	TenantID       string      `json:"tenant_id"`
	AddressType    AddressType `json:"address_type"`
	CurrentVersion int         `json:"current_version"`
	CreatedBy      string      `json:"created_by"`
	CreatedAt      time.Time   `json:"created_at"`
}

// AddressObservation is one verification observation of one version.
type AddressObservation struct {
	VerificationID string             `json:"verification_id,omitempty"`
	VersionNo      int                `json:"version_no"`
	Verifier       string             `json:"verifier"`
	Result         VerificationResult `json:"result"`
	Confidence     float64            `json:"confidence"`
	Findings       []string           `json:"findings"`
	NormalizedSeen string             `json:"normalized_seen"`
	ObservedBy     string             `json:"observed_by,omitempty"`
	ObservedAt     time.Time          `json:"observed_at"`
}

// AddressView is an address with the version in force at the requested date.
type AddressView struct {
	Address       Address              `json:"address"`
	Version       *AddressVersion      `json:"version"`
	Verifications []AddressObservation `json:"verifications"`
	History       []AddressVersion     `json:"history,omitempty"`
}

// AddressSummary is a search row.
type AddressSummary struct {
	AddressID   string       `json:"address_id"`
	AddressType AddressType  `json:"address_type"`
	VersionNo   int          `json:"version_no"`
	Locality    string       `json:"locality"`
	PostalCode  string       `json:"postal_code,omitempty"`
	Country     string       `json:"country"`
	State       AddressState `json:"verification_state"`
	Normalized  string       `json:"normalized"`
}

// AddressSearchQuery filters SearchAddresses.
type AddressSearchQuery struct {
	Text       string
	Country    string
	PostalCode string
	Type       AddressType
	Limit      int
	After      string
}

// AddressFields are the structured fields common to create and amend.
type AddressFields struct {
	Lines         []string    `json:"lines"`
	Locality      string      `json:"locality"`
	Subdivision   string      `json:"subdivision,omitempty"`
	PostalCode    string      `json:"postal_code,omitempty"`
	Country       string      `json:"country"`
	Formatted     string      `json:"formatted,omitempty"`
	ValidFrom     string      `json:"valid_from,omitempty"`
	SourceClass   SourceClass `json:"source_class"`
	EvidenceRef   string      `json:"evidence_ref"`
	CorrelationID string      `json:"correlation_id,omitempty"`
}

// Trim returns the fields with surrounding space removed; the source value is otherwise untouched.
func (f AddressFields) Trim() AddressFields {
	out := f
	out.Lines = nil
	for _, l := range f.Lines {
		out.Lines = append(out.Lines, strings.TrimSpace(l))
	}
	out.Locality = strings.TrimSpace(f.Locality)
	out.Subdivision = strings.TrimSpace(f.Subdivision)
	out.PostalCode = strings.TrimSpace(f.PostalCode)
	out.Country = strings.ToUpper(strings.TrimSpace(f.Country))
	out.Formatted = strings.TrimSpace(f.Formatted)
	out.EvidenceRef = strings.TrimSpace(f.EvidenceRef)
	return out
}

// Validate checks shape only. Whether the country exists or the postal code fits it is a verification finding,
// not a reason to refuse to record what the caller was told.
func (f AddressFields) Validate() error {
	t := f.Trim()
	if len(t.Lines) < 1 || len(t.Lines) > 4 {
		return fmt.Errorf("lines must hold 1 to 4 address lines")
	}
	for _, l := range t.Lines {
		if l == "" || len(l) > 200 {
			return fmt.Errorf("each address line must be 1 to 200 characters")
		}
	}
	if t.Locality == "" || len(t.Locality) > 100 {
		return fmt.Errorf("locality is required (1 to 100 characters)")
	}
	if len(t.Subdivision) > 100 || len(t.PostalCode) > 20 || len(t.Formatted) > 1000 {
		return fmt.Errorf("subdivision, postal_code or formatted is too long")
	}
	if !validCountry(t.Country) {
		return fmt.Errorf("country must be an ISO 3166-1 alpha-2 code shape, two upper-case letters")
	}
	if !f.SourceClass.Valid() {
		return fmt.Errorf("source_class is required")
	}
	if t.EvidenceRef == "" {
		return fmt.Errorf("evidence_ref is required")
	}
	if f.ValidFrom != "" {
		if _, err := ParseDate(f.ValidFrom); err != nil {
			return fmt.Errorf("valid_from %v", err)
		}
	}
	return nil
}

// Normalized is the comparison form of the fields.
func (f AddressFields) Normalized() string {
	t := f.Trim()
	return NormalizeAddress(t.Lines, t.Locality, t.Subdivision, t.PostalCode, t.Country)
}

// CreateAddressRequest is CreateAddress. create_request_id makes it idempotent.
type CreateAddressRequest struct {
	CreateRequestID string      `json:"create_request_id,omitempty"`
	AddressType     AddressType `json:"address_type"`
	AddressFields
}

// Validate checks the request. A REGISTERED address is a legal assertion and needs authority evidence (s14.2).
func (r CreateAddressRequest) Validate() error {
	if !r.AddressType.Valid() {
		return fmt.Errorf("address_type is not a known or custom (X_...) type")
	}
	if len(r.CreateRequestID) > 128 {
		return fmt.Errorf("create_request_id is too long")
	}
	if err := r.AddressFields.Validate(); err != nil {
		return err
	}
	if r.AddressType.NeedsAuthority() && !r.SourceClass.Authoritative() {
		return ErrRegisteredNeedsAuthority
	}
	return nil
}

// Fingerprint identifies what a create asks for, so a replayed id carrying different content is refused.
func (r CreateAddressRequest) Fingerprint() string {
	t := r.AddressFields.Trim()
	raw, _ := json.Marshal(struct {
		T                    string
		L                    []string
		C, S, P, K, F, V, SC string
	}{string(r.AddressType), t.Lines, t.Locality, t.Subdivision, t.PostalCode, t.Country, t.Formatted, t.ValidFrom, string(t.SourceClass)})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// AmendAddressRequest is AmendAddress: a new version of the same address. The type never changes: a billing
// address does not become a registered office by being edited.
type AmendAddressRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
	AddressFields
}

// Validate checks the request.
func (r AmendAddressRequest) Validate() error {
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("reason is required")
	}
	return r.AddressFields.Validate()
}

// VerifyAddressRequest is VerifyAddress.
type VerifyAddressRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Verifier        string `json:"verifier,omitempty"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// LinkAddressRequest is LinkAddressToParty. The link type must be the address's own type.
type LinkAddressRequest struct {
	AddressID             string      `json:"address_id"`
	LinkType              AddressType `json:"link_type"`
	ValidFrom             string      `json:"valid_from,omitempty"`
	EvidenceRef           string      `json:"evidence_ref"`
	ExpectedMasterVersion int64       `json:"expected_master_version,omitempty"`
	CorrelationID         string      `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r LinkAddressRequest) Validate() error {
	if strings.TrimSpace(r.AddressID) == "" {
		return fmt.Errorf("address_id is required")
	}
	if !r.LinkType.Valid() {
		return fmt.Errorf("link_type is not a known or custom (X_...) type")
	}
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required")
	}
	if r.ValidFrom != "" {
		if _, err := ParseDate(r.ValidFrom); err != nil {
			return fmt.Errorf("valid_from %v", err)
		}
	}
	return nil
}

// AddressLink is an address used by a party.
type AddressLink struct {
	LinkID      string      `json:"link_id"`
	AddressID   string      `json:"address_id"`
	PartyID     string      `json:"party_id"`
	LinkType    AddressType `json:"link_type"`
	ValidFrom   string      `json:"valid_from"`
	ValidTo     string      `json:"valid_to,omitempty"`
	EvidenceRef string      `json:"evidence_ref"`
	LinkedBy    string      `json:"linked_by"`
	LinkedAt    time.Time   `json:"linked_at"`
}

// PartyAddress is one address a party has or had, with the version in force at the date asked.
type PartyAddress struct {
	Link    AddressLink     `json:"link"`
	Version *AddressVersion `json:"version"`
}

var addressAbbrev = map[string]string{
	"STREET": "ST", "ROAD": "RD", "AVENUE": "AVE", "BOULEVARD": "BLVD", "DRIVE": "DR", "LANE": "LN",
	"COURT": "CT", "SQUARE": "SQ", "PLACE": "PL", "SUITE": "STE", "FLOOR": "FL", "BUILDING": "BLDG",
	"APARTMENT": "APT", "NORTH": "N", "SOUTH": "S", "EAST": "E", "WEST": "W", "PO": "POBOX",
}

// NormalizeAddress is the comparison form: upper case, accents folded, punctuation dropped, common suffixes
// abbreviated, the postal code without spaces or hyphens. It never replaces the source fields.
func NormalizeAddress(lines []string, locality, subdivision, postal, country string) string {
	part := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			switch {
			case r == '.' || r == '\'':
			case unicode.IsLetter(r) || unicode.IsDigit(r):
				b.WriteRune(foldRune(r))
			default:
				b.WriteRune(' ')
			}
		}
		words := strings.Fields(strings.ToUpper(b.String()))
		for i, w := range words {
			if a, ok := addressAbbrev[w]; ok {
				words[i] = a
			}
		}
		return strings.Join(words, " ")
	}
	var pc strings.Builder
	for _, r := range strings.ToUpper(postal) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			pc.WriteRune(r)
		}
	}
	return strings.Join([]string{part(strings.Join(lines, " ")), part(locality), part(subdivision), pc.String(),
		strings.ToUpper(strings.TrimSpace(country))}, "|")
}
