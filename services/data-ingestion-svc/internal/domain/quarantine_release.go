package domain

import (
	"fmt"
	"strings"
	"time"
)

// Governed release of RUN-LEVEL quarantine (ZS-SVC-Z-001 DQC-02, INV-11, INV-12, NP-16; INT-04).
//
// A run-level quarantine item is a whole batch that was refused. This service keeps a hash of its payload and never the
// records, so the batch cannot be re-queued: the way back is a corrected re-submission through CommitBatch. What is
// governed is that way back:
//
//   - someone states the correction made in the owning domain (RequestRunRelease);
//   - someone ELSE decides (DecideRunRelease), and an approval is never given by whoever quarantined the batch;
//   - the batch re-submitted under that approval must DIFFER from the one quarantined (a correction, not the same data
//     sent again), is validated like any batch, and consumes the approval when it lands;
//   - and the identical quarantined payload cannot be committed at all without that approved release.
//
// Every step is an append-only ledger entry that outlives the item moving on.

type RunReleaseKind string

const (
	RunReleaseRequested RunReleaseKind = "REQUESTED"
	RunReleaseRefused   RunReleaseKind = "REFUSED"
	RunReleaseApproved  RunReleaseKind = "APPROVED"
	RunReleaseConsumed  RunReleaseKind = "CONSUMED"
)

// RunReleaseState is where a run-level quarantine item stands.
type RunReleaseState string

const (
	RunQuarantined      RunReleaseState = "QUARANTINED"
	RunReleasePending   RunReleaseState = "RELEASE_PENDING"
	RunReleaseApprovedS RunReleaseState = "RELEASE_APPROVED"
	RunReleased         RunReleaseState = "RELEASED"
)

type RunReleaseEntry struct {
	LedgerID        string         `json:"ledger_id"`
	QuarantineID    string         `json:"quarantine_id"`
	Kind            RunReleaseKind `json:"kind"`
	Actor           string         `json:"actor"`
	Reason          string         `json:"reason"`
	CorrectionRef   string         `json:"correction_ref,omitempty"`
	AnswersLedgerID *string        `json:"answers_ledger_id,omitempty"`
	LandingID       string         `json:"landing_id,omitempty"`
	OccurredAt      time.Time      `json:"occurred_at"`
}

// DeriveRunReleaseState folds a ledger, in order, into the item's state.
func DeriveRunReleaseState(entries []RunReleaseEntry) RunReleaseState {
	state := RunQuarantined
	for _, e := range entries {
		switch e.Kind {
		case RunReleaseRequested:
			state = RunReleasePending
		case RunReleaseRefused:
			state = RunQuarantined
		case RunReleaseApproved:
			state = RunReleaseApprovedS
		case RunReleaseConsumed:
			state = RunReleased
		}
	}
	return state
}

type RunReleaseRequest struct {
	Reason        string `json:"reason"`
	CorrectionRef string `json:"correction_ref"`
}

func (r RunReleaseRequest) Validate() error {
	if strings.TrimSpace(r.Reason) == "" || strings.TrimSpace(r.CorrectionRef) == "" {
		return fmt.Errorf("%w: a release request needs a reason and a reference to the correction made in the owning domain", ErrRunReleaseInvalid)
	}
	if len(r.Reason) > 2000 || len(r.CorrectionRef) > 500 {
		return fmt.Errorf("%w: reason is at most 2000 characters and correction_ref at most 500", ErrRunReleaseInvalid)
	}
	return nil
}

type RunReleaseDecision struct {
	Reason string `json:"reason"`
}

func (r RunReleaseDecision) Validate() error {
	if strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 2000 {
		return fmt.Errorf("%w: a release decision needs a reason (at most 2000 characters)", ErrRunReleaseInvalid)
	}
	return nil
}

var (
	ErrRunReleaseInvalid         = errorString("invalid release request")
	ErrRunReleaseOpen            = errorString("a release request or an unused approval is already open for this quarantined batch")
	ErrRunNoOpenRelease          = errorString("this quarantined batch has no open release request")
	ErrRunReleaseNotIndependent  = errorString("a release request cannot be decided by the person who made it, and an approval cannot be given by the person who quarantined the batch")
	ErrRunReleaseAlreadyReleased = errorString("this quarantined batch has already been released")
	ErrRunReleaseRequired        = errorString("this payload is quarantined; it can only be re-submitted under an approved release")
	ErrRunReleaseNotApproved     = errorString("the quarantined batch has no approved, unused release")
	ErrRunReleaseNotACorrection  = errorString("the re-submitted batch is identical to the one quarantined; a release needs a corrected batch")
)
