// Package retry re-attempts deliveries that failed for a reason worth trying
// again, and gives up on the ones that are not.
//
// Until this package existed, internal/deliver classified every failure as
// retryable or settled and nothing read the classification. A greylisted
// payslip notice, an SMTP relay restarting mid-deploy and an
// identity-context-svc blip all concluded FAILED on the first attempt and
// stayed that way — indistinguishable, in the register, from a mailbox that
// does not exist.
package retry

import (
	"math"
	"math/rand"
	"time"
)

// Policy decides whether and when a failed delivery is attempted again.
type Policy struct {
	// MaxAttempts bounds total attempts, including the first one made
	// synchronously inside the send request. 1 disables retry entirely.
	MaxAttempts int

	// BaseDelay is the wait after the first failure; each subsequent wait
	// doubles it, up to MaxDelay.
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

// DefaultPolicy is five attempts over roughly half an hour: 30s, 1m, 2m, 4m.
//
// The shape is chosen for what actually fails here. Transient SMTP faults are
// short (a relay restarting, a connection reset) or deliberately timed
// (greylisting, which is conventionally a few minutes). Backing off past
// MaxDelay would mostly delay notices that were going to succeed on the second
// try, and a notification that has been unsent for half an hour is a problem
// for a person to look at, not for a timer to keep quietly retrying.
var DefaultPolicy = Policy{
	MaxAttempts: 5,
	BaseDelay:   30 * time.Second,
	MaxDelay:    8 * time.Minute,
}

// Normalize replaces nonsense with the default. A misconfigured retry policy
// should not be able to turn into an unbounded resend loop against somebody's
// mail server.
func (p Policy) Normalize() Policy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = DefaultPolicy.MaxAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = DefaultPolicy.BaseDelay
	}
	if p.MaxDelay < p.BaseDelay {
		p.MaxDelay = p.BaseDelay
	}
	return p
}

// NextAttempt returns when the next attempt is due, given how many have
// already been made, and whether there should be one at all.
//
// attemptsMade counts attempts that have concluded, so the value passed is the
// notification's delivery_attempts AFTER the failure being handled — the
// column the store has just incremented. attemptsMade >= MaxAttempts is
// exhaustion, and the caller concludes the notification FAILED.
func (p Policy) NextAttempt(now time.Time, attemptsMade int) (time.Time, bool) {
	p = p.Normalize()
	if attemptsMade >= p.MaxAttempts || attemptsMade < 1 {
		return time.Time{}, false
	}

	// 2^(n-1), guarded: at attemptsMade 63 the shift would overflow, and
	// float64 keeps the arithmetic honest before the cap brings it back.
	backoff := float64(p.BaseDelay) * math.Pow(2, float64(attemptsMade-1))
	if backoff > float64(p.MaxDelay) || math.IsInf(backoff, 0) {
		backoff = float64(p.MaxDelay)
	}
	delay := time.Duration(backoff)

	// Jitter up to 20%, spread over the wait rather than concentrated at its
	// end. Without it, a mail server that rejected fifty notices in one outage
	// gets all fifty back simultaneously the moment the backoff expires —
	// which is the same burst that caused the failure, retried in lockstep.
	jitter := time.Duration(rand.Int63n(int64(delay)/5 + 1))
	return now.Add(delay + jitter), true
}
