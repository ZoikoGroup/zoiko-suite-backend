package domain

// ORG-08 Establishment (ZS-SVC-I-001 s4.8): CreateEstablishment, ChangeEstablishmentStatus and the reads.
//
// An establishment is a legal-presence fact with its own id. It stands at an address but is not the address: an
// address proves no establishment, tax nexus or registration, and this model infers none. Status moves through a
// named lifecycle and every move is recorded, so what was active on a date can be reconstructed exactly.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// EstablishmentType is what kind of presence this is.
type EstablishmentType string

const (
	EstBranch               EstablishmentType = "BRANCH"
	EstPermanent            EstablishmentType = "PERMANENT_ESTABLISHMENT"
	EstPlaceOfBusiness      EstablishmentType = "PLACE_OF_BUSINESS"
	EstRepresentativeOffice EstablishmentType = "REPRESENTATIVE_OFFICE"
	EstOperatingSite        EstablishmentType = "OPERATING_SITE"
	EstEmploymentSite       EstablishmentType = "EMPLOYMENT_SITE"
)

// Valid reports whether t is a known establishment type.
func (t EstablishmentType) Valid() bool {
	switch t {
	case EstBranch, EstPermanent, EstPlaceOfBusiness, EstRepresentativeOffice, EstOperatingSite, EstEmploymentSite:
		return true
	}
	return false
}

// Material reports whether activating this type can carry legal or tax consequences, so that it needs a principal
// other than the one who proposed it.
func (t EstablishmentType) Material() bool { return t == EstBranch || t == EstPermanent }

// EstablishmentStatus is the lifecycle: PROPOSED, then ACTIVE; ACTIVE and INACTIVE alternate; CLOSED is final.
type EstablishmentStatus string

const (
	EstProposed EstablishmentStatus = "PROPOSED"
	EstActive   EstablishmentStatus = "ACTIVE"
	EstInactive EstablishmentStatus = "INACTIVE"
	EstClosed   EstablishmentStatus = "CLOSED"
)

// Valid reports whether s is a known status.
func (s EstablishmentStatus) Valid() bool {
	switch s {
	case EstProposed, EstActive, EstInactive, EstClosed:
		return true
	}
	return false
}

// CanMoveTo reports whether the lifecycle allows from -> to.
func (s EstablishmentStatus) CanMoveTo(to EstablishmentStatus) bool {
	switch s {
	case EstProposed:
		return to == EstActive || to == EstClosed
	case EstActive:
		return to == EstInactive || to == EstClosed
	case EstInactive:
		return to == EstActive || to == EstClosed
	}
	return false
}

// Establishment is the record.
type Establishment struct {
	EstablishmentID   string              `json:"establishment_id"`
	TenantID          string              `json:"tenant_id"`
	LegalEntityID     string              `json:"legal_entity_id"`
	EstablishmentType EstablishmentType   `json:"establishment_type"`
	Name              string              `json:"name"`
	AddressID         string              `json:"address_id"`
	Status            EstablishmentStatus `json:"status"`
	Material          bool                `json:"material"`
	ValidFrom         string              `json:"valid_from"`
	ValidTo           string              `json:"valid_to,omitempty"`
	SourceClass       SourceClass         `json:"source_class"`
	EvidenceRef       string              `json:"evidence_ref"`
	RecordVersion     int64               `json:"record_version"`
	CreatedBy         string              `json:"created_by"`
	CreatedAt         time.Time           `json:"created_at"`
	ActivatedBy       string              `json:"activated_by,omitempty"`
}

// EstablishmentTransition is one recorded move.
type EstablishmentTransition struct {
	Seq           int                 `json:"seq"`
	From          EstablishmentStatus `json:"from_status,omitempty"`
	To            EstablishmentStatus `json:"to_status"`
	EffectiveDate string              `json:"effective_date"`
	Command       string              `json:"command"`
	Reason        string              `json:"reason,omitempty"`
	EvidenceRef   string              `json:"evidence_ref"`
	Actor         string              `json:"actor"`
	VersionBefore int64               `json:"version_before,omitempty"`
	VersionAfter  int64               `json:"version_after"`
	OccurredAt    time.Time           `json:"occurred_at"`
}

// EstablishmentView is an establishment with the address version at it and, optionally, its history.
type EstablishmentView struct {
	Establishment Establishment             `json:"establishment"`
	Address       *AddressVersion           `json:"address"`
	History       []EstablishmentTransition `json:"history,omitempty"`
}

// CreateEstablishmentRequest is CreateEstablishment. The establishment starts PROPOSED.
type CreateEstablishmentRequest struct {
	CreateRequestID   string            `json:"create_request_id,omitempty"`
	LegalEntityID     string            `json:"legal_entity_id"`
	EstablishmentType EstablishmentType `json:"establishment_type"`
	Name              string            `json:"name"`
	AddressID         string            `json:"address_id"`
	ValidFrom         string            `json:"valid_from,omitempty"`
	SourceClass       SourceClass       `json:"source_class"`
	EvidenceRef       string            `json:"evidence_ref"`
	CorrelationID     string            `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r CreateEstablishmentRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.LegalEntityID) == "":
		return fmt.Errorf("legal_entity_id is required")
	case !r.EstablishmentType.Valid():
		return fmt.Errorf("establishment_type is not a known type")
	case strings.TrimSpace(r.Name) == "" || len(r.Name) > 200:
		return fmt.Errorf("name is required (1 to 200 characters)")
	case strings.TrimSpace(r.AddressID) == "":
		return fmt.Errorf("address_id is required")
	case !r.SourceClass.Valid():
		return fmt.Errorf("source_class is required")
	case strings.TrimSpace(r.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required: an address alone does not establish a presence")
	case len(r.CreateRequestID) > 128:
		return fmt.Errorf("create_request_id is too long")
	}
	if r.ValidFrom != "" {
		if _, err := ParseDate(r.ValidFrom); err != nil {
			return fmt.Errorf("valid_from %v", err)
		}
	}
	return nil
}

// Fingerprint identifies what a create asks for, so a replayed id carrying different content is refused.
func (r CreateEstablishmentRequest) Fingerprint() string {
	raw, _ := json.Marshal([]string{r.LegalEntityID, string(r.EstablishmentType), strings.TrimSpace(r.Name), r.AddressID,
		r.ValidFrom, string(r.SourceClass)})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ChangeEstablishmentStatusRequest is ChangeEstablishmentStatus. EffectiveDate defaults to today.
type ChangeEstablishmentStatusRequest struct {
	ExpectedVersion int64               `json:"expected_version"`
	To              EstablishmentStatus `json:"-"`
	EffectiveDate   string              `json:"effective_date,omitempty"`
	Reason          string              `json:"reason,omitempty"`
	EvidenceRef     string              `json:"evidence_ref"`
	CorrelationID   string              `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r ChangeEstablishmentStatusRequest) Validate() error {
	switch {
	case !r.To.Valid() || r.To == EstProposed:
		return fmt.Errorf("an establishment moves to ACTIVE, INACTIVE or CLOSED")
	case strings.TrimSpace(r.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required")
	case r.To != EstActive && strings.TrimSpace(r.Reason) == "":
		return fmt.Errorf("reason is required to deactivate or close an establishment")
	}
	if r.EffectiveDate != "" {
		if _, err := ParseDate(r.EffectiveDate); err != nil {
			return fmt.Errorf("effective_date %v", err)
		}
	}
	return nil
}

// EntityAddress is an address a legal entity has or had, with the version in force at the date asked.
type EntityAddress struct {
	Link    AddressLink     `json:"link"`
	Version *AddressVersion `json:"version"`
}

// StructuredOffice reads a registered office given as JSON into address fields. The ORG-03 field has always been
// opaque JSON, so this is lenient about key names; it succeeds only when the minimum for a structured address is
// there: a line, a locality and a two-letter country. Anything else stays opaque, as before.
func StructuredOffice(raw string) (AddressFields, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return AddressFields{}, false
	}
	low := map[string]any{}
	for k, v := range m {
		low[strings.ToLower(strings.TrimSpace(k))] = v
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := low[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	var f AddressFields
	if ls, ok := low["lines"].([]any); ok {
		for _, l := range ls {
			if s, ok := l.(string); ok && strings.TrimSpace(s) != "" {
				f.Lines = append(f.Lines, strings.TrimSpace(s))
			}
		}
	}
	if len(f.Lines) == 0 {
		for _, k := range []string{"line1", "address_line1", "street", "address1", "address"} {
			if s := str(k); s != "" {
				f.Lines = append(f.Lines, s)
				break
			}
		}
		for _, k := range []string{"line2", "address_line2", "address2", "line3", "address_line3"} {
			if s := str(k); s != "" {
				f.Lines = append(f.Lines, s)
			}
		}
	}
	f.Locality = str("locality", "city", "town")
	f.Subdivision = str("subdivision", "state", "region", "province", "county")
	f.PostalCode = str("postal_code", "postcode", "zip", "zip_code")
	f.Country = strings.ToUpper(str("country", "country_code"))
	if len(f.Lines) == 0 || len(f.Lines) > 4 || f.Locality == "" || !validCountry(f.Country) {
		return AddressFields{}, false
	}
	return f, true
}
