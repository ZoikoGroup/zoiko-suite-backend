package domain

// MDM-01 governed contact points (ZS-SVC-U-001): an email address a same-tenant party can be reached at.
//
// A contact point is proposed by one principal and verified on evidence by another; RETIRED is final. The address is
// stored in clear because delivery needs it, so it is protected by tenant isolation and the read permission, and it is
// never placed in an event: events carry the digest. Cross-tenant contact lookup is not modelled (MDM-I-05, MDM-I-29).

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"
)

// ContactChannel is how a party is reached. Only EMAIL is modelled.
type ContactChannel string

const ContactEmail ContactChannel = "EMAIL"

// Valid reports whether c is a known channel.
func (c ContactChannel) Valid() bool { return c == ContactEmail }

// ContactPurpose is what the contact point is for.
type ContactPurpose string

const (
	ContactGeneral     ContactPurpose = "GENERAL"
	ContactBilling     ContactPurpose = "BILLING"
	ContactLegalNotice ContactPurpose = "LEGAL_NOTICE"
)

// Valid reports whether p is a known purpose.
func (p ContactPurpose) Valid() bool {
	switch p {
	case ContactGeneral, ContactBilling, ContactLegalNotice:
		return true
	}
	return false
}

// ContactStatus is the lifecycle: PROPOSED, then VERIFIED; either may be RETIRED, which is final.
type ContactStatus string

const (
	ContactProposed ContactStatus = "PROPOSED"
	ContactVerified ContactStatus = "VERIFIED"
	ContactRetired  ContactStatus = "RETIRED"
)

// Valid reports whether s is a known status.
func (s ContactStatus) Valid() bool {
	return s == ContactProposed || s == ContactVerified || s == ContactRetired
}

// CanMoveTo reports whether the lifecycle allows from -> to.
func (s ContactStatus) CanMoveTo(to ContactStatus) bool {
	switch s {
	case ContactProposed:
		return to == ContactVerified || to == ContactRetired
	case ContactVerified:
		return to == ContactRetired
	}
	return false
}

// MaxContactValueLength is the longest email address (RFC 5321 path limit as practised).
const MaxContactValueLength = 254

// NormalizeEmail returns the stored form of an email address: trimmed, one address, no display name, domain
// lower-cased. The local part is kept as written, because it can be case-sensitive.
func NormalizeEmail(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	switch {
	case v == "":
		return "", fmt.Errorf("value is required")
	case len(v) > MaxContactValueLength:
		return "", fmt.Errorf("value is longer than %d characters", MaxContactValueLength)
	}
	for _, r := range v {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == ',' || r == ';' || r == '<' || r == '>' {
			return "", fmt.Errorf("value must be a single email address with no display name")
		}
	}
	a, err := mail.ParseAddress(v)
	if err != nil {
		return "", fmt.Errorf("value is not a valid email address")
	}
	if a.Name != "" || a.Address != v {
		return "", fmt.Errorf("value must be a single email address with no display name")
	}
	at := strings.LastIndex(v, "@")
	local, domainPart := v[:at], v[at+1:]
	if local == "" || !strings.Contains(domainPart, ".") || strings.HasPrefix(domainPart, ".") || strings.HasSuffix(domainPart, ".") {
		return "", fmt.Errorf("value is not a valid email address")
	}
	return local + "@" + strings.ToLower(domainPart), nil
}

// ContactDigest is the sha256 hex of a normalised value: the dedupe key, and what events carry instead of the value.
func ContactDigest(normalised string) string {
	sum := sha256.Sum256([]byte(normalised))
	return hex.EncodeToString(sum[:])
}

// ContactPoint is the record. Value is empty in a command outcome: only a read returns it.
type ContactPoint struct {
	ContactPointID string         `json:"contact_point_id"`
	TenantID       string         `json:"tenant_id"`
	PartyID        string         `json:"party_id"`
	Channel        ContactChannel `json:"channel"`
	Value          string         `json:"value,omitempty"`
	ValueDigest    string         `json:"value_digest"`
	Purpose        ContactPurpose `json:"purpose"`
	Status         ContactStatus  `json:"status"`
	SourceClass    SourceClass    `json:"source_class"`
	ValidFrom      time.Time      `json:"valid_from"`
	ValidTo        *time.Time     `json:"valid_to"`
	EvidenceRef    string         `json:"evidence_ref"`
	CreatedBy      string         `json:"created_by"`
	VerifiedBy     string         `json:"verified_by,omitempty"`
	VerifiedAt     *time.Time     `json:"verified_at"`
	RetiredBy      string         `json:"retired_by,omitempty"`
	RetiredAt      *time.Time     `json:"retired_at,omitempty"`
	RecordVersion  int64          `json:"record_version"`
	CreatedAt      time.Time      `json:"created_at"`
}

// WithoutValue returns the record with the address removed, for a command outcome.
func (c ContactPoint) WithoutValue() ContactPoint {
	c.Value = ""
	return c
}

// ContactPointItem is one row of the read: exactly what a notifier needs.
type ContactPointItem struct {
	ContactPointID string         `json:"contact_point_id"`
	PartyID        string         `json:"party_id"`
	Channel        ContactChannel `json:"channel"`
	Value          string         `json:"value"`
	Purpose        ContactPurpose `json:"purpose"`
	Status         ContactStatus  `json:"status"`
	ValidFrom      time.Time      `json:"valid_from"`
	ValidTo        *time.Time     `json:"valid_to"`
	SourceClass    SourceClass    `json:"source_class"`
	VerifiedAt     *time.Time     `json:"verified_at"`
	EvidenceRef    string         `json:"evidence_ref"`
	RecordVersion  int64          `json:"record_version"`
}

// Item is the read form of the record.
func (c ContactPoint) Item() ContactPointItem {
	return ContactPointItem{ContactPointID: c.ContactPointID, PartyID: c.PartyID, Channel: c.Channel, Value: c.Value, Purpose: c.Purpose,
		Status: c.Status, ValidFrom: c.ValidFrom, ValidTo: c.ValidTo, SourceClass: c.SourceClass, VerifiedAt: c.VerifiedAt,
		EvidenceRef: c.EvidenceRef, RecordVersion: c.RecordVersion}
}

// InForce reports whether the record is in force at t: valid_from <= t and (no end or t < valid_to).
func (c ContactPoint) InForce(t time.Time) bool {
	return !c.ValidFrom.After(t) && (c.ValidTo == nil || t.Before(*c.ValidTo))
}

// CreateContactPointRequest proposes a contact point. It starts PROPOSED.
type CreateContactPointRequest struct {
	Channel       ContactChannel `json:"channel"`
	Value         string         `json:"value"`
	Purpose       ContactPurpose `json:"purpose,omitempty"`
	SourceClass   SourceClass    `json:"source_class"`
	EvidenceRef   string         `json:"evidence_ref,omitempty"`
	ValidFrom     *time.Time     `json:"valid_from,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
}

// Normalized returns the request with the value normalised and the purpose defaulted, or the validation error.
func (r CreateContactPointRequest) Normalized() (CreateContactPointRequest, error) {
	if !r.Channel.Valid() {
		return r, fmt.Errorf("channel must be EMAIL")
	}
	if r.Purpose == "" {
		r.Purpose = ContactGeneral
	}
	if !r.Purpose.Valid() {
		return r, fmt.Errorf("purpose must be GENERAL, BILLING or LEGAL_NOTICE")
	}
	if !r.SourceClass.Valid() {
		return r, fmt.Errorf("source_class is required")
	}
	v, err := NormalizeEmail(r.Value)
	if err != nil {
		return r, err
	}
	r.Value = v
	r.EvidenceRef = strings.TrimSpace(r.EvidenceRef)
	return r, nil
}

// VerifyContactPointRequest verifies a PROPOSED contact point on evidence.
type VerifyContactPointRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	EvidenceRef     string `json:"evidence_ref"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r VerifyContactPointRequest) Validate() error {
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required to verify a contact point")
	}
	return nil
}

// RetireContactPointRequest retires a contact point, finally.
type RetireContactPointRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r RetireContactPointRequest) Validate() error {
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("reason is required to retire a contact point")
	}
	return nil
}

// ContactPointQuery filters the read. Zero AsOf means now; empty Status means every state.
type ContactPointQuery struct {
	Channel ContactChannel
	Status  ContactStatus
	AsOf    time.Time
}

// Validate checks the filters.
func (q ContactPointQuery) Validate() error {
	if q.Channel != "" && !q.Channel.Valid() {
		return fmt.Errorf("channel must be EMAIL")
	}
	if q.Status != "" && !q.Status.Valid() {
		return fmt.Errorf("status must be PROPOSED, VERIFIED or RETIRED")
	}
	return nil
}
