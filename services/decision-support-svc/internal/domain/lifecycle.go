package domain

import (
	"strconv"
	"strings"
)

// Status is the recommendation's output-disposition state.
//
// ZS-SVC-X-001 AIG-04 §7.3 (Figure 7 - Output disposition):
//
//	REVIEW_REQUIRED -> ACCEPTED   -> may feed authorized deterministic downstream action
//	                -> REJECTED   -> no downstream authority
//	                -> SUPERSEDED -> replacement output/review linked
//	"ACCEPTED does not itself execute the business action"
//
// plus NP-40: "Review deadline expires -> Escalate/expire; never auto-accept",
// which this service models as REVIEW_REQUIRED -> EXPIRED.
//
// Every state other than REVIEW_REQUIRED is terminal.
type Status string

const (
	StatusReviewRequired Status = "REVIEW_REQUIRED"
	StatusAccepted       Status = "ACCEPTED"
	StatusRejected       Status = "REJECTED"
	StatusExpired        Status = "EXPIRED"
	StatusSuperseded     Status = "SUPERSEDED"
)

// allowedTransitions is the whole state machine.
var allowedTransitions = map[Status][]Status{
	StatusReviewRequired: {StatusAccepted, StatusRejected, StatusExpired, StatusSuperseded},
	StatusAccepted:       {},
	StatusRejected:       {},
	StatusExpired:        {},
	StatusSuperseded:     {},
}

// ParseStatus returns the Status for s, or false if s is not one.
func ParseStatus(s string) (Status, bool) {
	status := Status(strings.ToUpper(strings.TrimSpace(s)))
	_, known := allowedTransitions[status]
	return status, known
}

// CanTransition reports whether the state machine allows from -> to.
func CanTransition(from, to Status) bool {
	for _, allowed := range allowedTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// IsTerminal reports whether no further transition is possible.
func (s Status) IsTerminal() bool {
	return len(allowedTransitions[s]) == 0
}

// OversightClass is the AIG-04 §7.1 oversight class the recommendation's
// eventual use requires.
//
//	O1 - User review:        identified user sees AI status and can reject/edit.
//	O2 - Qualified reviewer: "explicit accept/reject, evidence and no self-bypass".
//	O3 - Dual/control review: "maker-checker ... WFC segregation-of-duties / quorum".
//	O4 - AI prohibited:      "Technical AI block" — refused at creation.
//
// O2 is the default: when the caller does not say, treat the output as
// material (fail safe toward more oversight, not less).
type OversightClass string

const (
	OversightUserReview        OversightClass = "O1"
	OversightQualifiedReviewer OversightClass = "O2"
	OversightDualControlReview OversightClass = "O3"
	OversightAIProhibited      OversightClass = "O4"
	DefaultOversightClass                     = OversightQualifiedReviewer
)

// ParseOversightClass returns the class for s ("" means the default).
// O4 parses successfully so the caller can be told it is prohibited, not
// that it is unknown.
func ParseOversightClass(s string) (OversightClass, bool) {
	switch OversightClass(strings.ToUpper(strings.TrimSpace(s))) {
	case "":
		return DefaultOversightClass, true
	case OversightUserReview:
		return OversightUserReview, true
	case OversightQualifiedReviewer:
		return OversightQualifiedReviewer, true
	case OversightDualControlReview:
		return OversightDualControlReview, true
	case OversightAIProhibited:
		return OversightAIProhibited, true
	default:
		return "", false
	}
}

// RequiresIndependentReviewer is the segregation-of-duties rule: for a
// material recommendation (O2 or O3) the principal who requested it may not
// be the principal who ACCEPTS it ("no self-bypass", AIG-04 §7.1). Rejecting,
// expiring or superseding one's own request is allowed — none of those can
// lend AI output any authority.
func (c OversightClass) RequiresIndependentReviewer() bool {
	return c == OversightQualifiedReviewer || c == OversightDualControlReview
}

// RequiresApprovalReference is the O3 maker-checker rule: acceptance must
// cite the workflow/quorum approval that authorised it.
func (c OversightClass) RequiresApprovalReference() bool {
	return c == OversightDualControlReview
}

// ETagForVersion renders a version as a strong entity tag, e.g. "3".
func ETagForVersion(version int) string {
	return `"` + strconv.Itoa(version) + `"`
}

// ParseETag reads an If-Match value back into a version. It accepts a
// strong tag ("3"), a weak tag (W/"3") or a bare number (3).
func ParseETag(value string) (int, bool) {
	tag := strings.TrimSpace(value)
	tag = strings.TrimPrefix(tag, "W/")
	tag = strings.Trim(tag, `"`)
	version, err := strconv.Atoi(tag)
	if err != nil || version < 1 {
		return 0, false
	}
	return version, true
}
