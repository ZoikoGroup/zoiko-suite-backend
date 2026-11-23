package knowledge

import "strings"

// Doc 09 §16 editorial states, derived from an ingest report: content is
// written as DRAFT, fails VALIDATION or waits in PENDING_REVIEW for its two
// approvers, then becomes APPROVED (staged for a signed release).
const (
	EditorialValidationFailed = "VALIDATION_FAILED"
	EditorialPendingReview    = "PENDING_REVIEW"
	EditorialApproved         = "APPROVED"
)

// EditorialState maps an ingest outcome to the Doc 09 §16 editorial state.
// A draft that passes every check is PENDING_REVIEW, not APPROVED: only an
// approved source with its approvers stages for a release.
func EditorialState(r Report, draft bool) string {
	switch {
	case r.State == StateStaged && !draft:
		return EditorialApproved
	case r.State == StateStaged && draft:
		return EditorialPendingReview
	case r.State == StateHoldApproval && !strings.Contains(r.Reason, "overdue") && !strings.Contains(r.Reason, "owner may not"):
		return EditorialPendingReview
	default:
		return EditorialValidationFailed
	}
}
