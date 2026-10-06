package policy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"zoiko.io/notification-svc/internal/domain"
)

// The guard is the last gate for expiry: the first attempt, a retry and a resend all pass it.
func TestDirectGuard_AnExpiredCommunicationIsNeverSubmitted(t *testing.T) {
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Second), now.Add(time.Hour)
	run := func(expires *time.Time) (domain.DeliveryOutcome, *fakeInner, *fakeKill) {
		in, k := &fakeInner{}, &fakeKill{}
		g := guard(t, in, allowed(), k)
		g.now = func() time.Time { return now }
		n := email()
		n.ExpiresAt = expires
		return g.Deliver(context.Background(), n), in, k
	}

	out, in, k := run(&past)
	assert.False(t, out.Delivered)
	assert.False(t, out.Retryable, "time does not undo an expiry")
	assert.Zero(t, in.calls, "the provider is not called")
	assert.Zero(t, k.checked, "refused before anything else is consulted")
	assert.Equal(t, "NCD-015", out.BlockCode)
	assert.True(t, strings.HasPrefix(out.Reason, "NCD-015 DELIVERY_EXPIRED:"), out.Reason)

	out, _, _ = run(&now)
	assert.False(t, out.Delivered, "expiry is inclusive: at expires_at it is already too late")

	out, in, _ = run(&future)
	assert.True(t, out.Delivered)
	assert.Equal(t, 1, in.calls)
	out, _, _ = run(nil)
	assert.True(t, out.Delivered, "no expires_at, no expiry")
}
