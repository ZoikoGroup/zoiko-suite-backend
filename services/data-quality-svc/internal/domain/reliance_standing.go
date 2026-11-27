package domain

import "fmt"

// The reverse of the blast radius: given something that relied on certified evidence, which certifications does it
// rest on, and has any been withdrawn? A downstream owner asks this about its own object (a posting batch, a report)
// to find out whether what it built is still standing on valid evidence.

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

// ReliedCertification is one certification the object rests on, with its current standing.
type ReliedCertification struct {
	Reliance   CertificationReliance    `json:"reliance"`
	Status     string                   `json:"status"` // ACTIVE, STALE or REVOKED
	Revocation *CertificationRevocation `json:"revocation,omitempty"`
}

// RelianceStanding answers "is what I built still standing?". Withdrawn counts the certifications that have been
// revoked: the object rests on withdrawn evidence and its owner must act. Stale counts those past their validity
// window (not withdrawn, but no longer to be relied on for anything new).
type RelianceStanding struct {
	Subject        RelianceSubject       `json:"subject"`
	Count          int                   `json:"count"`
	Withdrawn      int                   `json:"withdrawn"`
	Stale          int                   `json:"stale"`
	Certifications []ReliedCertification `json:"certifications"`
}
