package domain

// ORG-09 Registration & License (ZS-SVC-I-001 s4.9, ZS-SVC-U-001 s13, s14.1).
//
// A registration is generic evidence that a holder is registered or licensed by an authority, and the lifecycle of
// that evidence. It is not a tax conclusion (TAX-01 owns tax-registration semantics), not a finding that the holder
// is legally established, and not proof until it is verified: an unverified registration is DRAFT and never reads
// as in force. Expiry is surfaced from the dates even before the sweep records it.

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

// RegistrationType is what kind of registration or licence this is.
type RegistrationType string

const (
	RegCompany       RegistrationType = "COMPANY_REGISTRATION"
	RegTaxReference  RegistrationType = "TAX_REFERENCE" // a reference only: TAX-01 owns tax-registration semantics
	RegEmployer      RegistrationType = "EMPLOYER_PAYROLL"
	RegPermit        RegistrationType = "PERMIT"
	RegLicense       RegistrationType = "LICENSE"
	RegRegulated     RegistrationType = "REGULATED"
	RegOtherGoverned RegistrationType = "OTHER_GOVERNED"
)

// Valid reports whether t is a known type.
func (t RegistrationType) Valid() bool {
	switch t {
	case RegCompany, RegTaxReference, RegEmployer, RegPermit, RegLicense, RegRegulated, RegOtherGoverned:
		return true
	}
	return false
}

// Material reports whether the type can carry legal consequences, so that verifying and renewing it needs a
// principal other than the one who created it or last made it valid.
func (t RegistrationType) Material() bool {
	return t == RegPermit || t == RegLicense || t == RegRegulated
}

// Sensitive reports whether numbers of this type are masked in reads and search unless revealed.
func (t RegistrationType) Sensitive() bool { return t == RegTaxReference || t == RegEmployer }

// RegistrationStatus is the lifecycle: DRAFT, then ACTIVE; ACTIVE and SUSPENDED alternate; EXPIRED, REVOKED and
// SUPERSEDED end it (an expired one can be renewed back to ACTIVE).
type RegistrationStatus string

const (
	RegDraft      RegistrationStatus = "DRAFT"
	RegActive     RegistrationStatus = "ACTIVE"
	RegSuspended  RegistrationStatus = "SUSPENDED"
	RegExpired    RegistrationStatus = "EXPIRED"
	RegRevoked    RegistrationStatus = "REVOKED"
	RegSuperseded RegistrationStatus = "SUPERSEDED"
)

// Valid reports whether s is a known status.
func (s RegistrationStatus) Valid() bool {
	switch s {
	case RegDraft, RegActive, RegSuspended, RegExpired, RegRevoked, RegSuperseded:
		return true
	}
	return false
}

// Live reports whether the status holds a candidate-unique number.
func (s RegistrationStatus) Live() bool {
	return s == RegDraft || s == RegActive || s == RegSuspended
}

// Registration is the record.
type Registration struct {
	RegistrationID      string             `json:"registration_id"`
	TenantID            string             `json:"tenant_id"`
	RegistrationType    RegistrationType   `json:"registration_type"`
	IssuingAuthority    string             `json:"issuing_authority"`
	Jurisdiction        string             `json:"jurisdiction"`
	Number              string             `json:"number"`
	Masked              bool               `json:"number_masked,omitempty"`
	HolderLegalEntityID string             `json:"holder_legal_entity_id,omitempty"`
	HolderPartyID       string             `json:"holder_party_id,omitempty"`
	Status              RegistrationStatus `json:"status"`
	Material            bool               `json:"material"`
	IssueDate           string             `json:"issue_date,omitempty"`
	EffectiveFrom       string             `json:"effective_from"`
	ExpiryDate          string             `json:"expiry_date,omitempty"`
	SourceClass         SourceClass        `json:"source_class"`
	EvidenceRef         string             `json:"evidence_ref"`
	EvidenceHash        string             `json:"evidence_hash,omitempty"`
	SupersedesID        string             `json:"supersedes_id,omitempty"`
	SupersededByID      string             `json:"superseded_by_id,omitempty"`
	RecordVersion       int64              `json:"record_version"`
	CreatedBy           string             `json:"created_by"`
	CreatedAt           time.Time          `json:"created_at"`
	VerifiedBy          string             `json:"verified_by,omitempty"`
	VerifiedAt          *time.Time         `json:"verified_at,omitempty"`
	LastGrantBy         string             `json:"-"`
	// InForce is what a consumer may rely on: ACTIVE, begun, and not past its expiry date. A DRAFT never is.
	InForce bool `json:"in_force"`
	// Lapsed is set when the dates say it expired but the sweep has not yet recorded it.
	Lapsed bool `json:"lapsed,omitempty"`
}

// Evaluate sets InForce and Lapsed as of the date.
func (r *Registration) Evaluate(asOf time.Time) {
	day := asOf.UTC().Format("2006-01-02")
	pastExpiry := r.ExpiryDate != "" && r.ExpiryDate < day
	r.Lapsed = (r.Status == RegActive || r.Status == RegSuspended) && pastExpiry
	r.InForce = r.Status == RegActive && r.EffectiveFrom <= day && !pastExpiry
}

// MaskNumber hides all but the last four characters of a sensitive number.
func MaskNumber(n string) string {
	r := []rune(n)
	if len(r) <= 6 {
		return strings.Repeat("*", len(r))
	}
	return strings.Repeat("*", len(r)-4) + string(r[len(r)-4:])
}

// RegistrationTransition is one recorded step of a registration's life.
type RegistrationTransition struct {
	Seq           int                `json:"seq"`
	Command       string             `json:"command"`
	From          RegistrationStatus `json:"from_status,omitempty"`
	To            RegistrationStatus `json:"to_status"`
	EffectiveDate string             `json:"effective_date"`
	ExpiryAfter   string             `json:"expiry_after,omitempty"`
	Reason        string             `json:"reason,omitempty"`
	EvidenceRef   string             `json:"evidence_ref"`
	EvidenceHash  string             `json:"evidence_hash,omitempty"`
	Actor         string             `json:"actor"`
	VersionBefore int64              `json:"version_before,omitempty"`
	VersionAfter  int64              `json:"version_after"`
	OccurredAt    time.Time          `json:"occurred_at"`
}

// RegistrationSearchQuery filters ListRegistrations.
type RegistrationSearchQuery struct {
	HolderLegalEntityID string
	HolderPartyID       string
	Type                RegistrationType
	Status              RegistrationStatus
	Jurisdiction        string
	Limit               int
	After               string
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CreateRegistrationRequest is CreateRegistration. The registration starts DRAFT: unverified.
type CreateRegistrationRequest struct {
	CreateRequestID     string           `json:"create_request_id,omitempty"`
	RegistrationType    RegistrationType `json:"registration_type"`
	IssuingAuthority    string           `json:"issuing_authority"`
	Jurisdiction        string           `json:"jurisdiction"`
	Number              string           `json:"number"`
	HolderLegalEntityID string           `json:"holder_legal_entity_id,omitempty"`
	HolderPartyID       string           `json:"holder_party_id,omitempty"`
	IssueDate           string           `json:"issue_date,omitempty"`
	EffectiveFrom       string           `json:"effective_from"`
	ExpiryDate          string           `json:"expiry_date,omitempty"`
	SourceClass         SourceClass      `json:"source_class"`
	EvidenceRef         string           `json:"evidence_ref"`
	EvidenceHash        string           `json:"evidence_hash,omitempty"`
	// SupersedesID names the registration this one replaces. It becomes SUPERSEDED when this one is verified.
	SupersedesID  string `json:"supersedes_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

func parseOptDate(label, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := ParseDate(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %v", label, err)
	}
	return t, nil
}

// Validate checks the request.
func (r CreateRegistrationRequest) Validate() error {
	switch {
	case !r.RegistrationType.Valid():
		return fmt.Errorf("registration_type is not a known type")
	case strings.TrimSpace(r.IssuingAuthority) == "" || len(r.IssuingAuthority) > 200:
		return fmt.Errorf("issuing_authority is required (1 to 200 characters)")
	case strings.TrimSpace(r.Jurisdiction) == "" || len(r.Jurisdiction) > 16:
		return fmt.Errorf("jurisdiction is required (a code of up to 16 characters)")
	case NormalizeRegistrationNumber(r.Number) == "" || len(r.Number) > 100:
		return fmt.Errorf("number is required (1 to 100 characters, with a letter or digit)")
	case (strings.TrimSpace(r.HolderLegalEntityID) == "") == (strings.TrimSpace(r.HolderPartyID) == ""):
		return fmt.Errorf("name exactly one holder: holder_legal_entity_id or holder_party_id")
	case !r.SourceClass.Valid():
		return fmt.Errorf("source_class is required")
	case strings.TrimSpace(r.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required")
	case r.EvidenceHash != "" && !hashPattern.MatchString(r.EvidenceHash):
		return fmt.Errorf("evidence_hash must be a lower-case hex SHA-256")
	case len(r.CreateRequestID) > 128:
		return fmt.Errorf("create_request_id is too long")
	}
	eff, err := ParseDate(r.EffectiveFrom)
	if err != nil {
		return fmt.Errorf("effective_from %v", err)
	}
	issue, err := parseOptDate("issue_date", r.IssueDate)
	if err != nil {
		return err
	}
	exp, err := parseOptDate("expiry_date", r.ExpiryDate)
	if err != nil {
		return err
	}
	if !issue.IsZero() && issue.After(eff) {
		return fmt.Errorf("issue_date is after effective_from")
	}
	if !exp.IsZero() && exp.Before(eff) {
		return fmt.Errorf("expiry_date is before effective_from")
	}
	return nil
}

// Fingerprint identifies what a create asks for, so a replayed id carrying different content is refused.
func (r CreateRegistrationRequest) Fingerprint() string {
	raw, _ := json.Marshal([]string{string(r.RegistrationType), NormalizeAuthority(r.IssuingAuthority), strings.ToUpper(strings.TrimSpace(r.Jurisdiction)),
		NormalizeRegistrationNumber(r.Number), r.HolderLegalEntityID, r.HolderPartyID, r.IssueDate, r.EffectiveFrom, r.ExpiryDate,
		string(r.SourceClass), r.SupersedesID})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// VerifyRegistrationRequest is VerifyRegistration: DRAFT or SUSPENDED to ACTIVE, on evidence.
type VerifyRegistrationRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	EvidenceRef     string `json:"evidence_ref"`
	EvidenceHash    string `json:"evidence_hash,omitempty"`
	Reason          string `json:"reason,omitempty"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r VerifyRegistrationRequest) Validate() error {
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required: a registration is verified from evidence")
	}
	if r.EvidenceHash != "" && !hashPattern.MatchString(r.EvidenceHash) {
		return fmt.Errorf("evidence_hash must be a lower-case hex SHA-256")
	}
	return nil
}

// RenewRegistrationRequest is RenewRegistration: a new expiry, on evidence.
type RenewRegistrationRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	NewExpiryDate   string `json:"new_expiry_date"`
	IssueDate       string `json:"issue_date,omitempty"`
	EffectiveFrom   string `json:"effective_from,omitempty"`
	EvidenceRef     string `json:"evidence_ref"`
	EvidenceHash    string `json:"evidence_hash,omitempty"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r RenewRegistrationRequest) Validate() error {
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required: a renewal is recorded from evidence")
	}
	if r.EvidenceHash != "" && !hashPattern.MatchString(r.EvidenceHash) {
		return fmt.Errorf("evidence_hash must be a lower-case hex SHA-256")
	}
	if _, err := ParseDate(r.NewExpiryDate); err != nil {
		return fmt.Errorf("new_expiry_date %v", err)
	}
	if _, err := parseOptDate("issue_date", r.IssueDate); err != nil {
		return err
	}
	if _, err := parseOptDate("effective_from", r.EffectiveFrom); err != nil {
		return err
	}
	return nil
}

// RegistrationEndRequest is SuspendRegistration, ExpireRegistration and RevokeRegistration: each needs a reason.
type RegistrationEndRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
	EvidenceRef     string `json:"evidence_ref"`
	EffectiveDate   string `json:"effective_date,omitempty"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r RegistrationEndRequest) Validate() error {
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("reason is required")
	}
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required")
	}
	if _, err := parseOptDate("effective_date", r.EffectiveDate); err != nil {
		return err
	}
	return nil
}

// NormalizeRegistrationNumber is the comparison form of a number: upper case letters and digits only, so
// "gb 123-456" and "GB123456" are one number. The source value is never replaced.
func NormalizeRegistrationNumber(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(foldRune(unicode.ToLower(r)))
		}
	}
	return strings.ToUpper(b.String())
}

// NormalizeAuthority is the comparison form of an issuing authority's name.
func NormalizeAuthority(s string) string {
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
	return strings.ToUpper(strings.Join(strings.Fields(b.String()), " "))
}
