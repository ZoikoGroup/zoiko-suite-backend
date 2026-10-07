package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

func TestPreferences_SetReadReplaceAndVersion(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx("tenant-pref")

	_, err := s.GetPreferences(ctx, "alice")
	assert.ErrorIs(t, err, domain.ErrPreferencesNotFound)

	p, err := s.SetPreferences(ctx, domain.SetPreferencesParams{PrincipalID: "alice", TimeZone: "Europe/London",
		QuietStart: strp("22:00"), QuietEnd: strp("07:00"), MutedChannels: []string{"SMS"}, UpdatedBy: "alice"})
	require.NoError(t, err)
	assert.Equal(t, 1, p.Version)
	require.NotNil(t, p.QuietStart)
	assert.Equal(t, "22:00", *p.QuietStart, "stored as a time of day and read back as HH:MM")
	assert.Equal(t, "07:00", *p.QuietEnd)
	assert.Equal(t, []string{"SMS"}, p.MutedChannels)

	got, err := s.GetPreferences(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, "Europe/London", got.TimeZone)

	// Replacing keeps one row and bumps the version; clearing quiet hours is allowed.
	p, err = s.SetPreferences(ctx, domain.SetPreferencesParams{PrincipalID: "alice", TimeZone: "Asia/Tokyo", UpdatedBy: "alice"})
	require.NoError(t, err)
	assert.Equal(t, 2, p.Version)
	assert.Nil(t, p.QuietStart)
	assert.Equal(t, []string{}, p.MutedChannels, "no muting reads back as an empty list, not null")
}

func TestPreferences_InvalidProfilesNeverReachTheDatabase(t *testing.T) {
	s := store.New(openTestPool(t))
	_, err := s.SetPreferences(tenantCtx("tenant-pref-bad"), domain.SetPreferencesParams{PrincipalID: "a", TimeZone: "Mars/Olympus", UpdatedBy: "a"})
	assert.ErrorIs(t, err, domain.ErrPreferencesInvalid)
}

func TestPreferences_DatabaseRefusesWhatTheServiceShouldHaveCaught(t *testing.T) {
	pool := openTestPool(t)
	for name, c := range map[string][2]string{
		"half a window":   {`INSERT INTO recipient_preferences (tenant_id, principal_id, time_zone, quiet_start, updated_by) VALUES ('t','p1','UTC','22:00','x')`, "ck_pref_quiet_pair"},
		"empty window":    {`INSERT INTO recipient_preferences (tenant_id, principal_id, time_zone, quiet_start, quiet_end, updated_by) VALUES ('t','p2','UTC','22:00','22:00','x')`, "ck_pref_quiet_window"},
		"offset as zone":  {`INSERT INTO recipient_preferences (tenant_id, principal_id, time_zone, updated_by) VALUES ('t','p3','+01:00','x')`, "ck_pref_tz_shape"},
		"muted not array": {`INSERT INTO recipient_preferences (tenant_id, principal_id, time_zone, muted_channels, updated_by) VALUES ('t','p4','UTC','{"a":1}','x')`, "ck_pref_muted_is_array"},
	} {
		conn, err := pool.Acquire(context.Background())
		require.NoError(t, err)
		_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id','t',false)`)
		require.NoError(t, err)
		_, err = conn.Exec(context.Background(), c[0])
		conn.Release()
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), c[1], name)
	}
}

func TestPreferences_TenantIsolated(t *testing.T) {
	s := store.New(openTestPool(t))
	_, err := s.SetPreferences(tenantCtx("tenant-pref-a"), domain.SetPreferencesParams{PrincipalID: "alice", TimeZone: "UTC", UpdatedBy: "alice"})
	require.NoError(t, err)
	_, err = s.GetPreferences(tenantCtx("tenant-pref-b"), "alice")
	assert.ErrorIs(t, err, domain.ErrPreferencesNotFound, "another tenant cannot see the profile")
}

func TestScheduleRetry_ADeferralIsStoredAsTheNextAttemptTime(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx("tenant-pref-defer")
	n := seedNotification(t, s, "tenant-pref-defer", "corr-pref-defer")
	until := time.Now().UTC().Add(7 * time.Hour).Truncate(time.Second)
	require.NoError(t, s.ScheduleRetry(ctx, n.NotificationID, "tenant-pref-defer", "NCD-012 QUIET_HOUR_DEFERRED", time.Now().UTC(), until,
		domain.AttemptMeta{Origin: domain.AttemptOriginRequest}))
	got, err := s.GetNotification(ctx, n.NotificationID)
	require.NoError(t, err)
	require.NotNil(t, got.NextAttemptAt)
	assert.WithinDuration(t, until, *got.NextAttemptAt, time.Second)
	assert.Equal(t, domain.StatusPending, got.Status, "held, not failed")
}

func TestMigration000023_DownThenUp(t *testing.T) {
	pool := openTestPool(t)
	for _, f := range []string{"000023_recipient_preferences.down.sql", "000023_recipient_preferences.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), string(b))
		require.NoError(t, err, f)
	}
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'recipient_preferences'`).Scan(&n))
	assert.Equal(t, 1, n)
}
