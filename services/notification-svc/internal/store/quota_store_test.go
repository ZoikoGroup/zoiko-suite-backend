package store_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/quota"
	"zoiko.io/notification-svc/internal/store"
)

// send creates one counted communication, as the direct send API does.
func send(s *store.PgStore, tenant, corr, recipient, class string) error {
	n := newNotification(tenant, "entity-1", recipient, corr)
	n.CommunicationClass = class
	_, err := s.CreateNotification(quota.WithCounting(tenantCtx(tenant)), n)
	return err
}

func exceeded(t *testing.T, err error) *quota.ExceededError {
	t.Helper()
	var ex *quota.ExceededError
	require.True(t, errors.As(err, &ex), "want a quota refusal, got %v", err)
	return ex
}

func countRows(t *testing.T, s *store.PgStore, tenant string) int {
	t.Helper()
	list, err := s.ListNotifications(tenantCtx(tenant), domain.ListFilter{LegalEntityID: "entity-1", Limit: 500})
	require.NoError(t, err)
	return len(list)
}

func TestQuota_APerRecipientBudgetRefusesAndLeavesNothingBehind(t *testing.T) {
	s := store.New(openAdminTestPool(t)).WithQuota(quota.Limits{RecipientPerHour: 3})
	tenant := "tenant-quota-r"

	for i := 0; i < 3; i++ {
		require.NoError(t, send(s, tenant, "corr-r-"+strconv.Itoa(i), "alice", "T0"))
	}
	err := send(s, tenant, "corr-r-over", "alice", "T0")
	ex := exceeded(t, err)
	assert.Equal(t, quota.DimRecipient, ex.Dimension)
	assert.Equal(t, 3, ex.Limit)
	assert.GreaterOrEqual(t, int(ex.RetryAfter.Seconds()), 1)

	assert.Equal(t, 3, countRows(t, s, tenant), "the refused send created nothing")
	// A refusal consumes nothing: hammering a full budget does not move the count past its limit.
	for i := 0; i < 5; i++ {
		exceeded(t, send(s, tenant, "corr-r-hammer-"+strconv.Itoa(i), "alice", "T0"))
	}
	assert.Equal(t, 3, countRows(t, s, tenant))

	// Another person is untouched, and so is another tenant's alice.
	require.NoError(t, send(s, tenant, "corr-r-bob", "bob", "T0"))
	require.NoError(t, send(s, "tenant-quota-r2", "corr-r-other", "alice", "T0"))
}

func TestQuota_SecurityMessagesHaveProtectedCapacityThatIsStillBounded(t *testing.T) {
	s := store.New(openAdminTestPool(t)).WithQuota(quota.Limits{TenantPerMinute: 2, TenantS0PerMinute: 3})
	tenant := "tenant-quota-s0"

	require.NoError(t, send(s, tenant, "g1", "p1", "A1"))
	require.NoError(t, send(s, tenant, "g2", "p2", "T0"))
	ex := exceeded(t, send(s, tenant, "g3", "p3", "A1"))
	assert.Equal(t, quota.DimTenant, ex.Dimension)
	assert.Equal(t, 2, ex.Limit)

	// The general pool is exhausted, and security mail still goes: its capacity is its own.
	for i := 0; i < 3; i++ {
		require.NoError(t, send(s, tenant, "s"+strconv.Itoa(i), "p"+strconv.Itoa(i), "S0"))
	}
	// But it is bounded too, against an abusive loop.
	ex = exceeded(t, send(s, tenant, "s-over", "p9", "S0"))
	assert.Equal(t, 3, ex.Limit)
}

func TestQuota_APerIntentBudgetIsSeparateFromOtherIntents(t *testing.T) {
	s := store.New(openAdminTestPool(t)).WithQuota(quota.Limits{IntentPerMinute: 2})
	tenant := "tenant-quota-i"
	a := regulatedIntentVersion(t, s, tenant, "legal.quota.a")
	b := regulatedIntentVersion(t, s, tenant, "legal.quota.b")
	withIntent := func(corr, recipient string, v *domain.IntentVersion) error {
		n := newNotification(tenant, "entity-1", recipient, corr)
		n.IntentVersionID = v.VersionID
		_, err := s.CreateNotification(quota.WithCounting(tenantCtx(tenant)), n)
		return err
	}
	require.NoError(t, withIntent("i1", "p1", a))
	require.NoError(t, withIntent("i2", "p2", a))
	ex := exceeded(t, withIntent("i3", "p3", a))
	assert.Equal(t, quota.DimIntent, ex.Dimension)
	require.NoError(t, withIntent("i4", "p4", b), "a different intent has its own budget")
	require.NoError(t, send(s, tenant, "plain", "p5", "T0"), "and an unbound send is not counted against an intent")
}

func TestQuota_AReplayIsNotCountedTwice(t *testing.T) {
	s := store.New(openAdminTestPool(t)).WithQuota(quota.Limits{RecipientPerHour: 1})
	tenant := "tenant-quota-replay"
	require.NoError(t, send(s, tenant, "same-event", "alice", "T0"))
	// The same source event again is a replay of the original, not a new send: it is not counted
	// and not refused, even though the budget is full.
	require.NoError(t, send(s, tenant, "same-event", "alice", "T0"))
	exceeded(t, send(s, tenant, "new-event", "alice", "T0"))
}

func TestQuota_OnlyCountedCreationsDrawOnTheBudget(t *testing.T) {
	s := store.New(openAdminTestPool(t)).WithQuota(quota.Limits{RecipientPerHour: 1})
	tenant := "tenant-quota-optin"
	// Creations that do not ask to be counted (notice dispatch, ledger register rows) are not.
	for i := 0; i < 3; i++ {
		n := newNotification(tenant, "entity-1", "alice", "uncounted-"+strconv.Itoa(i))
		_, err := s.CreateNotification(tenantCtx(tenant), n)
		require.NoError(t, err)
	}
	require.NoError(t, send(s, tenant, "counted-1", "alice", "T0"), "none of those used the budget")
	exceeded(t, send(s, tenant, "counted-2", "alice", "T0"))
}

func TestQuota_OffUnlessEnabled(t *testing.T) {
	s := store.New(openAdminTestPool(t)) // no WithQuota
	for i := 0; i < 30; i++ {
		require.NoError(t, send(s, "tenant-quota-off", "o"+strconv.Itoa(i), "alice", "T0"))
	}
}

func TestQuota_WindowsRollOverAndOldCountersAreRemoved(t *testing.T) {
	pool := openAdminTestPool(t)
	now := time.Date(2026, 10, 6, 9, 30, 10, 0, time.UTC)
	s := store.New(pool).WithQuota(quota.Limits{TenantPerMinute: 2}).WithQuotaClock(func() time.Time { return now })
	tenant := "tenant-quota-win"

	require.NoError(t, send(s, tenant, "w1", "p1", "T0"))
	require.NoError(t, send(s, tenant, "w2", "p2", "T0"))
	ex := exceeded(t, send(s, tenant, "w3", "p3", "T0"))
	assert.Equal(t, 50*time.Second, ex.RetryAfter, "the window ends at 09:31:00")

	now = now.Add(ex.RetryAfter) // the next window
	require.NoError(t, send(s, tenant, "w4", "p4", "T0"), "the budget frees up when the window ends")

	// A day later the first windows are cleaned up as a new one opens.
	now = now.Add(26 * time.Hour)
	require.NoError(t, send(s, tenant, "w5", "p5", "T0"))
	var rows int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM send_quota_counters WHERE tenant_id = $1`, tenant).Scan(&rows))
	assert.Equal(t, 1, rows, "yesterday's counters are gone")
}

// Backpressure preserves priority (6.5): when more is due than one batch carries, security goes
// first, then transactional, then operational, whatever order they became due in.
func TestPriority_DueSecurityMessagesAreFoundBeforeRoutineOnes(t *testing.T) {
	s := store.New(openAdminTestPool(t))
	tenant := "tenant-priority"
	mk := func(corr, class string, ago time.Duration) *domain.Notification {
		n := newNotification(tenant, "entity-1", "p-"+corr, corr)
		n.CommunicationClass = class
		due := time.Now().UTC().Add(-ago)
		n.NextAttemptAt = &due
		_, err := s.CreateNotification(tenantCtx(tenant), n)
		require.NoError(t, err)
		return n
	}
	routine := mk("routine", "A1", 3*time.Hour) // oldest
	txn := mk("txn", "T0", 2*time.Hour)
	unclassed := mk("unclassed", "", time.Hour)
	security := mk("security", "S0", time.Minute) // newest

	due, err := s.FindDueRetries(context.Background(), time.Now().UTC(), 10)
	require.NoError(t, err)
	var order []string
	for _, d := range due {
		order = append(order, d.NotificationID)
	}
	assert.Equal(t, []string{security.NotificationID, txn.NotificationID, unclassed.NotificationID, routine.NotificationID}, order,
		"S0, then T0 (oldest first, an unclassified send counts as T0), then A1")

	// A batch of one still takes the security message: nothing urgent waits behind the backlog.
	one, err := s.FindDueRetries(context.Background(), time.Now().UTC(), 1)
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, security.NotificationID, one[0].NotificationID)
}

func TestMigration000029_DownThenUp(t *testing.T) {
	pool := openAdminTestPool(t)
	for _, f := range []string{"000029_send_quota_counters.down.sql", "000029_send_quota_counters.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), string(b))
		require.NoError(t, err, f)
	}
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'send_quota_counters'`).Scan(&n))
	assert.Equal(t, 1, n)
}
