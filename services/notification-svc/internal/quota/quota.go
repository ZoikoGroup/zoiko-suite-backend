// Package quota decides how many communications one tenant, one recipient or one intent may
// create in a window (ZS-SVC-Y-001 NCD-03 section 6.5, "rate, abuse and storm controls").
//
// Quotas protect two things at once: the people receiving messages (a runaway loop must not
// bury one person in alerts) and the sender's reputation with the providers. Two rules shape
// the design:
//
//   - Security messages (S0) have PROTECTED capacity: their own budgets, which no other class
//     can use up, so a flood of reminders cannot stop a password reset. They are still bounded,
//     against abusive loops, but at higher limits.
//   - A refusal is explicit and consumes nothing. The caller is told which budget was hit and
//     when to retry; a refused send leaves no communication and no count behind, so a flood of
//     rejected requests cannot extend its own lockout.
//
// This package is pure: which budgets apply to a send, and when a window ends. Counting is the
// store's job, because a count shared by every replica has to live in the database.
package quota

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Dimensions a budget can be scoped to.
const (
	DimTenant    = "tenant"
	DimRecipient = "recipient"
	DimIntent    = "intent"
)

// Limits are the per-window maxima. Zero means "no limit on this budget".
type Limits struct {
	// Per tenant and channel, per minute. S0 has its own pool.
	TenantPerMinute   int
	TenantS0PerMinute int
	// Per recipient, per hour. S0 has its own, higher, allowance.
	RecipientPerHour   int
	RecipientS0PerHour int
	// Per intent, per minute (applies only to intent-bound sends).
	IntentPerMinute int
}

// DefaultLimits are safe starting points for an environment that has turned quotas on. They
// are deliberately conservative for a single person (20 messages an hour) and generous for a
// tenant (600 a minute); an environment with a larger legitimate batch raises them explicitly.
var DefaultLimits = Limits{
	TenantPerMinute: 600, TenantS0PerMinute: 200,
	RecipientPerHour: 20, RecipientS0PerHour: 60,
	IntentPerMinute: 300,
}

// Budget is one counter a send draws on.
type Budget struct {
	Dimension string
	Bucket    string // the counter's key, scoped within a tenant
	Window    time.Duration
	Limit     int
}

// Request describes the send being counted.
type Request struct {
	Class       string // S0, T0 or A1; empty is treated as T0
	Channel     string
	RecipientID string
	IntentID    string // empty when the send is not bound to an intent
}

// Protected reports whether a class draws on the protected security pool.
func Protected(class string) bool { return class == "S0" }

// Budgets lists the budgets a send draws on under the limits. A zero limit omits its budget.
func Budgets(l Limits, r Request) []Budget {
	protected := Protected(r.Class)
	pool := "general"
	tenantLimit, recipientLimit := l.TenantPerMinute, l.RecipientPerHour
	if protected {
		pool, tenantLimit, recipientLimit = "s0", l.TenantS0PerMinute, l.RecipientS0PerHour
	}
	var out []Budget
	if tenantLimit > 0 {
		out = append(out, Budget{DimTenant, fmt.Sprintf("tenant:%s:%s", strings.ToUpper(r.Channel), pool), time.Minute, tenantLimit})
	}
	if recipientLimit > 0 && r.RecipientID != "" {
		out = append(out, Budget{DimRecipient, fmt.Sprintf("recipient:%s:%s:%s", strings.ToUpper(r.Channel), pool, r.RecipientID), time.Hour, recipientLimit})
	}
	if l.IntentPerMinute > 0 && r.IntentID != "" {
		// Intent budgets are per pool too: a security intent's budget is not drawn down by anything else.
		out = append(out, Budget{DimIntent, fmt.Sprintf("intent:%s:%s", pool, r.IntentID), time.Minute, l.IntentPerMinute})
	}
	return out
}

// WindowStart is the start of the fixed window containing t.
func WindowStart(t time.Time, window time.Duration) time.Time {
	return t.UTC().Truncate(window)
}

// RetryAfter is how long until the window containing t ends, which is when the budget frees up.
func RetryAfter(t time.Time, window time.Duration) time.Duration {
	d := WindowStart(t, window).Add(window).Sub(t.UTC())
	if d < time.Second {
		return time.Second
	}
	return d
}

// ExceededError is returned when a send would go over a budget.
type ExceededError struct {
	Dimension  string
	Limit      int
	Window     time.Duration
	RetryAfter time.Duration
}

func (e *ExceededError) Error() string {
	return fmt.Sprintf("the %s send quota of %d per %s is exhausted; retry in %d seconds",
		e.Dimension, e.Limit, e.Window, int(e.RetryAfter.Seconds()))
}

type ctxKey struct{}

// WithCounting marks a context so that creating a communication under it is counted against
// the send quotas. It is opt-in per call, deliberately: the direct send API is a caller's choice
// to send and is counted, while notice dispatch (a regulated notice must not be refused for
// volume it did not cause) and the ledger pipeline's register rows (counted by their own path)
// are not.
func WithCounting(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, true)
}

// Counting reports whether the context asks for the send to be counted.
func Counting(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKey{}).(bool)
	return v
}
