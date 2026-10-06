package domain

import (
	"fmt"
	"time"
)

// MaxScheduleHorizon bounds how far ahead a send may be queued. A communication waiting for
// months is a decision somebody should revisit, not a timer.
const MaxScheduleHorizon = 90 * 24 * time.Hour

// ErrCancelNotAllowed means a communication can no longer be withdrawn: it was already
// submitted, is being submitted now, or has concluded.
var ErrCancelNotAllowed = errorString("only a queued communication that has not been submitted can be cancelled")

// ErrCancelReasonRequired: a withdrawal is evidence, so it states why.
var ErrCancelReasonRequired = errorString("a cancellation needs a reason")

// ScheduleProblem explains what is wrong with a send's timing.
type ScheduleProblem struct{ Msg string }

func (p ScheduleProblem) Error() string { return p.Msg }

// ValidateSchedule checks not_before and expires_at for a send on a channel. Timing is for
// EMAIL: an in-app notice is delivered by being recorded, so there is nothing to hold or expire.
func ValidateSchedule(channel string, notBefore, expiresAt *time.Time, now time.Time) error {
	if notBefore == nil && expiresAt == nil {
		return nil
	}
	if channel != ChannelEmail {
		return ScheduleProblem{"not_before and expires_at apply to EMAIL sends only"}
	}
	if notBefore != nil && notBefore.After(now.Add(MaxScheduleHorizon)) {
		return ScheduleProblem{fmt.Sprintf("not_before may be at most %d days ahead", int(MaxScheduleHorizon.Hours()/24))}
	}
	if expiresAt != nil && !expiresAt.After(now) {
		return ScheduleProblem{"expires_at must be in the future"}
	}
	if notBefore != nil && expiresAt != nil && !expiresAt.After(*notBefore) {
		return ScheduleProblem{"expires_at must be after not_before"}
	}
	return nil
}

// Queued reports whether a send waits for a future not_before.
func Queued(notBefore *time.Time, now time.Time) bool {
	return notBefore != nil && notBefore.After(now)
}
