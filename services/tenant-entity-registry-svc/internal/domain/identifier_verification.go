package domain

// ORG-09 / MDM-03 identifier verification (ZS-SVC-U-001 s13.2, s13.3, s14.1).
//
// A verification is an observation: what a source said about an identifier, when, with what evidence. It does not
// certify future validity. DecideObservation is the one place the rules live, as a pure function of the identifier's
// current state and the observation, so the pipeline's decisions can be tested without a database:
//
//   - a source verifies an identifier only if it is strong enough (EXTERNAL_PROVIDER or an authority), its
//     observation is fresh, and it is not older than what was already applied: never last-write-wins;
//   - a higher authority than the one that verified it may take over the verification;
//   - a higher authority that disagrees with a verified identifier opens a case and leaves it CONFLICTED, it never
//     overwrites the verified state;
//   - anything else is recorded and changes nothing.

import (
	"fmt"
	"strings"
	"time"
)

// Observation results.
const (
	ObsMatch    = "MATCH"
	ObsMismatch = "MISMATCH"
	ObsNotFound = "NOT_FOUND"
)

// Identifier verification statuses beyond UNVERIFIED and VERIFIED.
const (
	IdentUnverified = "UNVERIFIED"
	IdentVerified   = "VERIFIED"
	IdentFailed     = "FAILED"
	// IdentConflicted is a verified identifier a higher-authority source has disputed: not to be relied on until a
	// steward decides.
	IdentConflicted = "CONFLICTED"
)

// What recording an observation did.
const (
	EffectVerified       = "VERIFIED"
	EffectSourceUpgraded = "SOURCE_UPGRADED"
	EffectFailed         = "FAILED"
	EffectConflictOpened = "CONFLICT_OPENED"
	EffectNoChange       = "NO_CHANGE"
)

// CaseVerificationConflict is the stewardship case a disputed verification opens.
const CaseVerificationConflict = "VERIFICATION_CONFLICT"

// DecisionAcceptObservation settles a verification conflict in favour of the higher-authority observation: the
// identifier is FAILED. DecisionKeepVerified settles it the other way.
const DecisionAcceptObservation = "ACCEPT_OBSERVATION"

// LEI alignment findings (s13.3).
const (
	LEIAligned       = "ALIGNED"
	LEIProfileAbsent = "PROFILE_ABSENT"
	LEIDiffers       = "DIFFERS"
	LEINotLinked     = "NOT_LINKED"
	LEINotApplicable = "NOT_APPLICABLE"
)

// SourceRank orders sources by authority. A tenant's own assertion is the weakest.
func SourceRank(c SourceClass) int {
	switch c {
	case SourceAuthorityRegistry:
		return 4
	case SourceAuthorityDocument:
		return 3
	case SourceExternalProvider:
		return 2
	case SourceSystemImport:
		return 1
	}
	return 0
}

// canVerify reports whether a source is strong enough to verify an identifier: a provider or an authority.
func canVerify(c SourceClass) bool { return SourceRank(c) >= 2 }

// FreshnessWindow is how old an observation may be and still be applied. An LEI is published daily by its LOU, so
// its window is short; other identifiers change rarely.
func FreshnessWindow(scheme IdentifierScheme) time.Duration {
	if scheme == SchemeLEI {
		return 90 * 24 * time.Hour
	}
	return 365 * 24 * time.Hour
}

// leiLive reports whether a GLEIF registration status means the LEI is currently valid. Any other status is
// recorded, and verifies nothing.
func leiLive(status string) bool { return status == "ISSUED" }

// VerifyIdentifierRequest is POST /v1/identifiers/{id}:verify: one observation. The service never calls an
// authority itself (no connector logic here, s14.1 step 3); whoever called the authority through the exchange layer, or
// a steward holding the evidence, records what it said.
type VerifyIdentifierRequest struct {
	ExpectedMasterVersion int64       `json:"expected_master_version"`
	Result                string      `json:"result"`
	SourceClass           SourceClass `json:"source_class"`
	SourceRef             string      `json:"source_ref"`
	SourceVersion         string      `json:"source_version,omitempty"`
	ExchangeRef           string      `json:"exchange_ref,omitempty"`
	ObservedAt            time.Time   `json:"observed_at"`
	AuthorityStatus       string      `json:"authority_status,omitempty"`
	EvidenceRef           string      `json:"evidence_ref"`
	EvidenceHash          string      `json:"evidence_hash,omitempty"`
	CorrelationID         string      `json:"correlation_id,omitempty"`
}

// Validate checks the request against now.
func (r VerifyIdentifierRequest) Validate(now time.Time) error {
	switch {
	case r.Result != ObsMatch && r.Result != ObsMismatch && r.Result != ObsNotFound:
		return fmt.Errorf("result must be MATCH, MISMATCH or NOT_FOUND")
	case !r.SourceClass.Valid():
		return fmt.Errorf("source_class is required")
	case strings.TrimSpace(r.SourceRef) == "":
		return fmt.Errorf("source_ref is required: say which authority or provider answered")
	case strings.TrimSpace(r.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required")
	case r.ObservedAt.IsZero():
		return fmt.Errorf("observed_at is required")
	case r.ObservedAt.After(now.Add(5 * time.Minute)):
		return fmt.Errorf("observed_at is in the future")
	case r.EvidenceHash != "" && !hashPattern.MatchString(r.EvidenceHash):
		return fmt.Errorf("evidence_hash must be a lower-case hex SHA-256")
	case len(r.AuthorityStatus) > 24 || len(r.SourceRef) > 200 || len(r.SourceVersion) > 100 || len(r.ExchangeRef) > 200:
		return fmt.Errorf("source_ref, source_version, exchange_ref or authority_status is too long")
	}
	return nil
}

// IdentifierObservation is one recorded observation.
type IdentifierObservation struct {
	ObservationID   string      `json:"observation_id"`
	IdentifierID    string      `json:"identifier_id"`
	Result          string      `json:"result"`
	SourceClass     SourceClass `json:"source_class"`
	SourceRef       string      `json:"source_ref"`
	SourceVersion   string      `json:"source_version,omitempty"`
	ExchangeRef     string      `json:"exchange_ref,omitempty"`
	ObservedAt      time.Time   `json:"observed_at"`
	AuthorityStatus string      `json:"authority_status,omitempty"`
	EvidenceRef     string      `json:"evidence_ref"`
	EvidenceHash    string      `json:"evidence_hash,omitempty"`
	Effect          string      `json:"effect"`
	EffectNote      string      `json:"effect_note"`
	CaseID          string      `json:"case_id,omitempty"`
	StatusBefore    string      `json:"status_before"`
	StatusAfter     string      `json:"status_after"`
	RecordedBy      string      `json:"recorded_by"`
	RecordedAt      time.Time   `json:"recorded_at"`
}

// VerificationState is what is known about an identifier before an observation is applied.
type VerificationState struct {
	Status         string
	SourceClass    SourceClass // the source that verified it; empty unless VERIFIED or CONFLICTED
	LastObservedAt *time.Time
}

// ObservationDecision is what recording an observation does to the identifier.
type ObservationDecision struct {
	Effect      string
	NewStatus   string
	NewSource   SourceClass
	Note        string
	Conflict    bool
	AdvanceTime bool // set last_observed_at to the observation's time
}

// DecideObservation applies s14.1 steps 5 to 7 to one observation.
func DecideObservation(cur VerificationState, scheme IdentifierScheme, req VerifyIdentifierRequest, now time.Time) ObservationDecision {
	keep := func(note string) ObservationDecision {
		return ObservationDecision{Effect: EffectNoChange, NewStatus: cur.Status, NewSource: cur.SourceClass, Note: note}
	}
	rank := SourceRank(req.SourceClass)
	curRank := -1
	if cur.Status == IdentVerified || cur.Status == IdentConflicted {
		curRank = SourceRank(cur.SourceClass)
	}

	if !canVerify(req.SourceClass) {
		return keep("a " + string(req.SourceClass) + " observation cannot verify an identifier; it is recorded as evidence only")
	}
	if now.Sub(req.ObservedAt) > FreshnessWindow(scheme) {
		return keep("the observation is older than the freshness window for this scheme; it is recorded and not applied")
	}
	// Not last-write-wins: an observation older than what was already applied does not undo it, unless a higher
	// authority is correcting a verified identifier.
	if cur.LastObservedAt != nil && req.ObservedAt.Before(*cur.LastObservedAt) && !(cur.Status == IdentVerified && rank > curRank) {
		return keep("the observation is older than the one already applied")
	}
	if cur.Status == IdentConflicted {
		return keep("a verification conflict is open for this identifier; a steward decides it")
	}

	switch req.Result {
	case ObsMatch:
		if scheme == SchemeLEI && !leiLive(req.AuthorityStatus) {
			return keep("the LOU reports the LEI as " + orNone(req.AuthorityStatus) + ", not ISSUED; recorded, not verified")
		}
		if cur.Status == IdentVerified {
			if rank > curRank {
				return ObservationDecision{Effect: EffectSourceUpgraded, NewStatus: IdentVerified, NewSource: req.SourceClass, AdvanceTime: true,
					Note: "verified by a higher authority than before"}
			}
			return keep("already verified by an equal or higher source")
		}
		return ObservationDecision{Effect: EffectVerified, NewStatus: IdentVerified, NewSource: req.SourceClass, AdvanceTime: true,
			Note: "the source confirms the identifier"}

	default: // MISMATCH or NOT_FOUND
		if cur.Status == IdentVerified {
			if rank > curRank {
				return ObservationDecision{Effect: EffectConflictOpened, NewStatus: IdentConflicted, NewSource: cur.SourceClass, Conflict: true, AdvanceTime: true,
					Note: "a higher authority disagrees with the verified identifier: a stewardship case is open and the identifier is not overwritten"}
			}
			return keep("the observation does not outrank the verification in force; recorded, not applied")
		}
		return ObservationDecision{Effect: EffectFailed, NewStatus: IdentFailed, AdvanceTime: true,
			Note: "the source does not confirm the identifier"}
	}
}

func orNone(s string) string {
	if s == "" {
		return "unstated"
	}
	return s
}

// LEICheckDigits returns the two check digits for an 18-character LEI base.
func LEICheckDigits(base18 string) string {
	return fmt.Sprintf("%02d", 98-leiMod97(base18+"00"))
}

func leiMod97(s string) int {
	rem := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			rem = (rem*10 + int(r-'0')) % 97
		case r >= 'A' && r <= 'Z':
			rem = (rem*100 + int(r-'A') + 10) % 97
		default:
			return -1
		}
	}
	return rem
}
