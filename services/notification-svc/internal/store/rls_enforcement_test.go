package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/quota"
	"zoiko.io/notification-svc/internal/store"
)

// The test pool connects as a superuser, and a superuser bypasses row-level security even when
// it is FORCEd. Every other "tenant isolated" test in this package therefore proves that each
// query filters by tenant, not that the database enforces it. This test proves the database:
// it seeds a tenant's data through the real store, then switches the session to the
// NOSUPERUSER NOBYPASSRLS role the harness creates and reads every table this work added WITHOUT
// a tenant filter in the query, so only the policy can hide another tenant's rows.
func TestRLS_NewTablesAreEnforcedByTheDatabaseNotJustByTheQueries(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool).WithQuota(quota.Limits{TenantPerMinute: 100})
	const a, b = "tenant-rls-a", "tenant-rls-b"
	ctxA := tenantCtx(a)

	// Seed every table, as tenant A, through the real code paths.
	_, err := s.SetPreferences(ctxA, domain.SetPreferencesParams{PrincipalID: "alice", TimeZone: "UTC", UpdatedBy: "alice"})
	require.NoError(t, err)

	iv := regulatedIntentVersion(t, s, a, "legal.rls")                                             // communication_intents + versions
	n, err := s.CreateNotice(ctxA, noticeParams(iv, domain.AckReceipt, time.Now().Add(time.Hour))) // regulated_notices + events
	require.NoError(t, err)
	_, attemptID := deliverNotice(t, s, a, n, "<rls@example.com>")      // notification_delivery_attempts
	mailboxAccepted(t, s, a, attemptID, domain.EvidenceMailboxAccepted) // notification_delivery_evidence
	_, err = s.RefreshNotice(ctxA, n.NoticeID, time.Now())              // -> ACK_PENDING
	require.NoError(t, err)
	_, _, err = s.RecordNoticeAck(ctxA, n.NoticeID, noticeRecipient, domain.ActionAcknowledge, "", time.Now()) // notice_acknowledgements
	require.NoError(t, err)
	require.NoError(t, send(s, a, "corr-rls-quota", "alice", "T0")) // send_quota_counters

	tables := []string{
		"recipient_preferences", "communication_intents", "communication_intent_versions",
		"regulated_notices", "regulated_notice_events", "notice_acknowledgements",
		"notification_delivery_attempts", "notification_delivery_evidence", "send_quota_counters",
	}

	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	defer conn.Release()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SET LOCAL ROLE zoiko_app_test")
	require.NoError(t, err)

	count := func(tbl string) int {
		var n int
		require.NoError(t, tx.QueryRow(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n), tbl) // no tenant filter, on purpose
		return n
	}
	setTenant := func(id string) {
		_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", id)
		require.NoError(t, err)
	}

	setTenant(a)
	for _, tbl := range tables {
		assert.Positive(t, count(tbl), "%s: the owning tenant must see its own rows", tbl)
	}
	setTenant(b)
	for _, tbl := range tables {
		assert.Zero(t, count(tbl), "%s: another tenant must see nothing, by policy", tbl)
	}
	setTenant("") // no tenant context at all
	for _, tbl := range tables {
		assert.Zero(t, count(tbl), "%s: no tenant context sees nothing (fail closed)", tbl)
	}

	// Writes are fenced too: tenant B cannot plant a row in tenant A's name (WITH CHECK).
	setTenant(b)
	_, err = tx.Exec(ctx, `SAVEPOINT plant`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO recipient_preferences (tenant_id, principal_id, time_zone, updated_by) VALUES ($1, 'mallory', 'UTC', 'mallory')`, a)
	assert.Error(t, err, "a tenant cannot write a row belonging to another tenant")
	_, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT plant`)
	require.NoError(t, err)
	// ...and cannot rewrite or remove what it cannot see.
	tag, err := tx.Exec(ctx, `UPDATE regulated_notices SET locale = 'xx'`)
	require.NoError(t, err)
	assert.Zero(t, tag.RowsAffected(), "tenant B updates nothing of tenant A's")
	tag, err = tx.Exec(ctx, `DELETE FROM send_quota_counters`)
	require.NoError(t, err)
	assert.Zero(t, tag.RowsAffected(), "tenant B deletes nothing of tenant A's")
}

// The cross-tenant discovery hatch used by the sweepers is read-only: it can find open notices
// but can never be used to change one.
func TestRLS_PlatformScopeCanReadOpenNoticesButNeverWriteThem(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	const a = "tenant-rls-hatch"
	iv := regulatedIntentVersion(t, s, a, "legal.rls.hatch")
	n, err := s.CreateNotice(tenantCtx(a), noticeParams(iv, domain.AckReceipt, time.Now().Add(time.Hour)))
	require.NoError(t, err)
	deliverNotice(t, s, a, n, "<rls-hatch@example.com>")

	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	defer conn.Release()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SET LOCAL ROLE zoiko_app_test")
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)")
	require.NoError(t, err)

	var seen int
	require.NoError(t, tx.QueryRow(ctx, `SELECT COUNT(*) FROM regulated_notices WHERE status = 'DELIVERY_IN_PROGRESS'`).Scan(&seen))
	assert.Equal(t, 1, seen, "the sweeper can discover open notices across tenants")

	tag, err := tx.Exec(ctx, `UPDATE regulated_notices SET locale = 'xx'`)
	require.NoError(t, err)
	assert.Zero(t, tag.RowsAffected(), "but the hatch can change nothing")
	var other int
	require.NoError(t, tx.QueryRow(ctx, `SELECT COUNT(*) FROM recipient_preferences`).Scan(&other))
	assert.Zero(t, other, "and it opens no other table")
}
