package domain

import "time"

// ZS-JUR-001 Wave 2: the data a runtime resolver loads. Defined here so the
// store (which reads it) and the resolver (which consumes it) do not depend
// on each other.

// EligiblePack is a pack version a resolver may consider. Scope is the set
// of jurisdiction codes the registry recorded for the version.
type EligiblePack struct {
	PackVersionID string
	PackRef       string
	Version       string
	Status        string
	EffectiveFrom time.Time // start of the pack's window (UTC midnight of the manifest date)
	EffectiveTo   *time.Time
	ScopeCodes    []string
}

// InForce reports whether the pack's own effective window contains t
// (start inclusive, end exclusive).
func (p EligiblePack) InForce(t time.Time) bool {
	return !t.Before(p.EffectiveFrom) && (p.EffectiveTo == nil || t.Before(*p.EffectiveTo))
}

// Covers reports whether the pack's scope names the jurisdiction code.
func (p EligiblePack) Covers(code string) bool {
	for _, c := range p.ScopeCodes {
		if c == code {
			return true
		}
	}
	return false
}

// LoadedCertification is the certification record plus the key it names.
type LoadedCertification struct {
	CertificationID string
	ArtifactDigest  string
	ReportJSON      string
	ReportDigest    string
	Signature       string
	KeyRef          string
	Key             *TrustedKey
}

// LoadedPack is everything needed to decide whether an artifact may be
// trusted, read from the registry in one go.
type LoadedPack struct {
	PackVersionID  string
	ArtifactJSON   string
	ArtifactDigest string
	VersionDigest  string // artifact_digest recorded on the pack version row
	Signature      *string
	KeyRef         *string
	Key            *TrustedKey
	Cert           *LoadedCertification
}

// Resolver evidence errors.
var (
	ErrIdempotencyConflict = errorString("the idempotency key was already used for a different request")
	ErrDecisionNotFound    = errorString("rule decision not found")
)

// Wave 7 release and deployment errors.
var (
	ErrInvalidLifecycle   = errorString("the pack version is not in a state that allows this command")
	ErrRingNotFound       = errorString("deployment ring not found")
	ErrRegionNotFound     = errorString("deployment region not found")
	ErrDeploymentNotFound = errorString("no active deployment of this version in that ring and region")
	ErrHotfixNotFound     = errorString("no hotfix is declared for this pack version")
	ErrNoticeNotFound     = errorString("source-change notice not found")
)

// ErrNotAssignedReviewer is returned when someone other than the assigned
// reviewer tries to close a source-change notice.
var ErrNotAssignedReviewer = errorString("only the assigned independent reviewer may close this notice")

// Wave 4 calendar and obligation errors.
var (
	ErrCalendarNotFound       = errorString("regulatory calendar or calendar version not found")
	ErrObligationRuleNotFound = errorString("obligation rule not found")
	ErrSubmissionNotFound     = errorString("regulatory submission not found")
	ErrRolloutNotFound        = errorString("jurisdiction rollout not found")
)
