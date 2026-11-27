package domain

import "fmt"

// The reverse of the blast radius: given something that relied on a certification, which certifications does it rest
// on, and has any been withdrawn? A downstream owner asks this about its own object (a posting batch, a report) to find
// out whether what it built is still standing on valid evidence. Scoped to one legal entity, because authorization in
// this service is per entity: the answer never spans entities.

// RelianceSubject identifies the relying object.
type RelianceSubject struct {
	RelyingService    string `json:"relying_service"`
	RelyingObjectType string `json:"relying_object_type"`
	RelyingObjectID   string `json:"relying_object_id"`
}

func (s RelianceSubject) Validate() error {
	for _, f := range []struct{ name, v string }{
		{"relying_service", s.RelyingService}, {"relying_object_type", s.RelyingObjectType}, {"relying_object_id", s.RelyingObjectID},
	} {
		if !relianceTokenRe.MatchString(f.v) {
			return fmt.Errorf("%w: %s must be 1-128 characters of letters, digits and _ . : / -", ErrRelianceInvalid, f.name)
		}
	}
	return nil
}

// ReliedCertification is one certification the object rests on, with its current standing: CERTIFIED (in force),
// REVOKED (withdrawn) or SUPERSEDED (replaced by a corrected certification).
type ReliedCertification struct {
	Reliance           CertificationReliance    `json:"reliance"`
	CertificationState CertificationState       `json:"certification_state"`
	Revocation         *CertificationRevocation `json:"revocation,omitempty"`
}

// RelianceStanding answers "is what I built still standing?". Withdrawn counts revoked certifications: the object rests
// on withdrawn evidence and its owner must act. Superseded counts certifications replaced by a corrected one: not
// withdrawn, but the object rests on evidence that has since been redone.
type RelianceStanding struct {
	LegalEntityID  string                `json:"legal_entity_id"`
	Subject        RelianceSubject       `json:"subject"`
	Count          int                   `json:"count"`
	Withdrawn      int                   `json:"withdrawn"`
	Superseded     int                   `json:"superseded"`
	Certifications []ReliedCertification `json:"certifications"`
}
