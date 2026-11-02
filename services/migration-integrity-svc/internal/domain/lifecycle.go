package domain

import (
	"errors"
	"strings"
	"time"
)

// ─── Lifecycle rules ─────────────────────────────────────────────────────────
//
// Each rule is a small pure function returning one of the sentinel errors
// below; the handler maps each sentinel to exactly one HTTP status (see
// handler/problem.go), and the store re-checks the state rules under a row
// lock so a concurrent request cannot slip past them.

var (
	// ErrJobArchived: the run is archived, so its quarantine items can no
	// longer be dispositioned. 409.
	ErrJobArchived = errors.New("migration job is archived; its quarantine items can no longer be dispositioned")
	// ErrJobAlreadyArchived: archiving twice is an illegal transition. 409.
	ErrJobAlreadyArchived = errors.New("migration job is already archived")
	// ErrEntryAlreadyDecided: REMEDIATED and WAIVED are terminal. 409.
	ErrEntryAlreadyDecided = errors.New("audit entry has already been dispositioned; its decision is immutable")

	// ErrSubmitterCannotDisposition: segregation of duties — whoever
	// submitted the run cannot dispose of its quarantine items. 403.
	ErrSubmitterCannotDisposition = errors.New("segregation of duties: the principal who submitted the validation run cannot disposition its quarantine items")
	// ErrBlockerOwnerOnly: an MQ0 needs executive/domain owner resolution. 403.
	ErrBlockerOwnerOnly = errors.New("an MQ0 blocker can only be resolved by its owner")

	// ErrOwnerIsSubmitter: an exception owner equal to the submitter would
	// make every MQ0 of the run unresolvable under the two rules above. 422.
	ErrOwnerIsSubmitter = errors.New("segregation of duties: exception_owner_id must name a principal other than the submitter")
	// ErrBlockerCannotBeWaived: MQ0's disposition is "stop the batch", not an
	// exception. 422.
	ErrBlockerCannotBeWaived = errors.New("an MQ0 blocker cannot be waived; it must be remediated")
	// ErrWaiverExpiryNotInFuture: a waiver must still be in force when granted. 422.
	ErrWaiverExpiryNotInFuture = errors.New("waiver_expires_at must be in the future")
)

// CheckSubmissionRules applies the identity-dependent rules of
// POST /validate: the exception owner must not be the submitter.
func CheckSubmissionRules(req *ValidateMigrationRequest, submitterID string) error {
	if strings.TrimSpace(req.ExceptionOwnerID) == strings.TrimSpace(submitterID) {
		return ErrOwnerIsSubmitter
	}
	return nil
}

// CanArchive reports whether a job in this status may be archived.
func CanArchive(status JobStatus) error {
	if status == JobStatusArchived {
		return ErrJobAlreadyArchived
	}
	return nil
}

// CanDisposition reports whether an entry in this state, on a job in this
// state, may move out of QUARANTINED.
func CanDisposition(jobStatus JobStatus, entryStatus EntryStatus) error {
	if jobStatus == JobStatusArchived {
		return ErrJobArchived
	}
	if entryStatus.IsDecided() {
		return ErrEntryAlreadyDecided
	}
	return nil
}

// CheckDispositionRules applies, in order, every rule a disposition must
// satisfy beyond the request's shape:
//
//  1. state machine (job not archived, entry still QUARANTINED)      409
//  2. segregation of duties (submitter cannot disposition)           403
//  3. MQ0 owner-only resolution                                      403
//  4. MQ0 cannot be waived                                           422
//  5. a waiver's expiry must be in the future                        422
//
// Jobs recorded before requested_by existed carry no submitter, and entries
// recorded before owners existed carry no owner; for those the rule that
// needs the missing fact is skipped rather than invented.
func CheckDispositionRules(job *MigrationJob, entry *AuditEntry, principalID string, req *RemediateRequest, now time.Time) error {
	if err := CanDisposition(job.Status, entry.Status); err != nil {
		return err
	}
	if job.RequestedBy != "" && job.RequestedBy == principalID {
		return ErrSubmitterCannotDisposition
	}
	if entry.Severity.Blocks() && entry.OwnerID != "" && entry.OwnerID != principalID {
		return ErrBlockerOwnerOnly
	}
	if req.TargetStatus() == EntryStatusWaived {
		if entry.Severity.Blocks() {
			return ErrBlockerCannotBeWaived
		}
		if !req.WaiverExpiresAt.After(now) {
			return ErrWaiverExpiryNotInFuture
		}
	}
	return nil
}

// CountOpenEntries returns how many entries are still QUARANTINED.
func CountOpenEntries(entries []AuditEntry) int {
	open := 0
	for _, entry := range entries {
		if !entry.Status.IsDecided() {
			open++
		}
	}
	return open
}
