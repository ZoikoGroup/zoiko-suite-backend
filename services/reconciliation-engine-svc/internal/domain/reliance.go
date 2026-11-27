package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Reliance on a certification, and the blast radius of revoking it (ZS-SVC-Z-001 RB-05, DQC-017). The same model as
// data-quality-svc's: a downstream service RECORDS that it relied on a certification, and revoking the certification
// names exactly that set. A reliance is an append-only fact, idempotent by its natural key, and can only be recorded
// while the run's certification is still CERTIFIED (not revoked, not superseded).

const PrefixReliance = "rrl_"

var relianceTokenRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)

const maxReliancePurposeLen = 500

var (
	ErrRelianceInvalid  = errorString("invalid reliance")
	ErrRelianceInactive = errorString("a certification that is not in force (revoked, superseded or never issued) cannot be relied on")
)

type RecordRelianceRequest struct {
	// RelyingService acted on the certified evidence; RelyingObjectType and RelyingObjectID say what it produced or
	// decided (for example gl-svc / journal-batch / jb_123).
	RelyingService    string `json:"relying_service"`
	RelyingObjectType string `json:"relying_object_type"`
	RelyingObjectID   string `json:"relying_object_id"`
	Purpose           string `json:"purpose"`
}

func (r RecordRelianceRequest) Validate() error {
	for _, f := range []struct{ name, v string }{
		{"relying_service", r.RelyingService}, {"relying_object_type", r.RelyingObjectType}, {"relying_object_id", r.RelyingObjectID},
	} {
		if !relianceTokenRe.MatchString(f.v) {
			return fmt.Errorf("%w: %s must be 1-128 characters of letters, digits and _ . : / -", ErrRelianceInvalid, f.name)
		}
	}
	if strings.TrimSpace(r.Purpose) == "" || len(r.Purpose) > maxReliancePurposeLen || strings.ContainsAny(r.Purpose, "\x00\r\n") {
		return fmt.Errorf("%w: purpose is required (at most %d characters, one line)", ErrRelianceInvalid, maxReliancePurposeLen)
	}
	return nil
}

type CertificationReliance struct {
	RelianceID        string    `json:"reliance_id"`
	TenantID          string    `json:"tenant_id"`
	CertificationID   string    `json:"certification_id"`
	RunID             string    `json:"run_id"`
	RelyingService    string    `json:"relying_service"`
	RelyingObjectType string    `json:"relying_object_type"`
	RelyingObjectID   string    `json:"relying_object_id"`
	Purpose           string    `json:"purpose"`
	RecordedBy        string    `json:"recorded_by"`
	RecordedAt        time.Time `json:"recorded_at"`
}

// BlastRadius is everything recorded as relying on a run's certification, with the certification's state. For a
// REVOKED certification these are the things that now rest on withdrawn evidence; the revocation is prospective, so
// none of them is changed, they are identified so their owners can act.
type BlastRadius struct {
	CertificationID    string                   `json:"certification_id"`
	RunID              string                   `json:"run_id"`
	CertificationState CertificationState       `json:"certification_state"`
	Revocation         *CertificationRevocation `json:"revocation,omitempty"`
	Count              int                      `json:"count"`
	ByService          map[string]int           `json:"by_service"`
	Reliances          []CertificationReliance  `json:"reliances"`
}
