// ORG-01 reads against a real Postgres (TEST_DATABASE_URL, like pg_store_test.go). The assurance history is
// captured by triggers and the authentication events are one query over three tables, so neither is exercised
// by anything but the database.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/federation"
	"zoiko.io/identity-context-svc/internal/store"
)

func mustExec(t *testing.T, r *fedRig, q string, args ...any) {
	t.Helper()
	_, err := r.admin.Exec(context.Background(), q, args...)
	require.NoError(t, err)
}

func TestIdentity_AssuranceHistoryIsCapturedByTheDatabase(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	now := time.Now().UTC().Truncate(time.Millisecond)

	require.NoError(t, r.s.CreateAuthSession(ctx, authSession("sid-1", "p-1", "t-1", now)))
	mustExec(t, r, `UPDATE auth_sessions SET client_id = 'other' WHERE sid = 'sid-1'`)
	items, _, err := r.s.ListAssuranceObservations(ctx, "t-1", "p-1", nil, 50)
	require.NoError(t, err)
	require.Len(t, items, 1, "a change to an unrelated column records nothing")
	assert.Equal(t, "ESTABLISHED", items[0].ChangeKind)
	assert.Equal(t, "SINGLE_FACTOR", items[0].AssuranceLevel)
	assert.Empty(t, items[0].PreviousLevel)
	assert.Equal(t, []string{"pwd"}, items[0].AuthMethods)
	assert.Equal(t, "sid-1", items[0].SessionID)

	mustExec(t, r, `UPDATE auth_sessions SET assurance_level = 'MFA', amr = ARRAY['pwd','otp'] WHERE sid = 'sid-1'`)
	mustExec(t, r, `UPDATE auth_sessions SET assurance_level = 'SINGLE_FACTOR', amr = ARRAY['pwd'] WHERE sid = 'sid-1'`)
	mustExec(t, r, `UPDATE auth_sessions SET amr = ARRAY['pwd','fed'] WHERE sid = 'sid-1'`)

	items, _, err = r.s.ListAssuranceObservations(ctx, "t-1", "p-1", nil, 50)
	require.NoError(t, err)
	require.Len(t, items, 4)
	// newest first
	assert.Equal(t, []string{"METHODS_CHANGED", "DOWNGRADE", "STEP_UP", "ESTABLISHED"},
		[]string{items[0].ChangeKind, items[1].ChangeKind, items[2].ChangeKind, items[3].ChangeKind})
	assert.Equal(t, "SINGLE_FACTOR", items[1].AssuranceLevel)
	assert.Equal(t, "MFA", items[1].PreviousLevel)
	assert.Equal(t, "SINGLE_FACTOR", items[2].PreviousLevel)
	assert.Equal(t, []string{"pwd", "otp"}, items[2].AuthMethods)
}

func TestIdentity_StepUpIsRecordedOnlyWhenVerified(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	now := time.Now().UTC()
	require.NoError(t, r.s.CreateAuthSession(ctx, authSession("sid-1", "p-1", "t-1", now.Add(-time.Hour))))

	log := func(id, action, outcome, basis string) {
		mustExec(t, r, `INSERT INTO access_decision_log (decision_log_id, principal_id, tenant_id, action_type,
			decision_outcome, decision_basis, correlation_id, decided_at) VALUES ($1::text,'p-1','t-1',$2,$3,$4,'corr-'||$1::text,$5)`,
			id, action, outcome, basis, now)
	}
	log("d-denied", "MFA_STEP_UP", "DENIED", "invalid_code")
	log("d-other", "MFA_ENROL", "ALLOWED", "totp_enrolled")
	items, _, err := r.s.ListAssuranceObservations(ctx, "t-1", "p-1", nil, 50)
	require.NoError(t, err)
	require.Len(t, items, 1, "a refused step-up and another action raise no assurance")

	log("d-ok", "MFA_STEP_UP", "ALLOWED", "totp_verified")
	items, _, err = r.s.ListAssuranceObservations(ctx, "t-1", "p-1", nil, 50)
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "STEP_UP", items[0].ChangeKind)
	assert.Equal(t, "MFA", items[0].AssuranceLevel)
	assert.Equal(t, "SINGLE_FACTOR", items[0].PreviousLevel, "the step-up says what it raised")
	assert.Equal(t, "corr-d-ok", items[0].CorrelationID)
	assert.Equal(t, []string{"pwd", "otp"}, items[0].AuthMethods)
}

func TestIdentity_AssuranceObservationsAreAppendOnlyAndTenantScoped(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	require.NoError(t, r.s.CreateAuthSession(ctx, authSession("sid-1", "p-1", "t-1", time.Now().UTC())))

	_, err := r.admin.Exec(ctx, `UPDATE assurance_observations SET assurance_level = 'PHISHING_RESISTANT'`)
	assert.Error(t, err, "history is never edited, even by an administrator")
	_, err = r.admin.Exec(ctx, `DELETE FROM assurance_observations`)
	assert.Error(t, err, "history is never deleted")

	items, _, err := r.s.ListAssuranceObservations(ctx, "t-2", "p-1", nil, 50)
	require.NoError(t, err)
	assert.Empty(t, items, "another tenant sees nothing")
}

func TestIdentity_AssuranceHistoryPagesWithoutRepeatsOrGaps(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	require.NoError(t, r.s.CreateAuthSession(ctx, authSession("sid-1", "p-1", "t-1", time.Now().UTC())))
	for i := 0; i < 4; i++ {
		level, amr := "MFA", "ARRAY['pwd','otp']"
		if i%2 == 1 {
			level, amr = "SINGLE_FACTOR", "ARRAY['pwd']"
		}
		mustExec(t, r, `UPDATE auth_sessions SET assurance_level = '`+level+`', amr = `+amr+` WHERE sid = 'sid-1'`)
	}

	var seen []string
	var after *store.Cursor
	for pages := 0; pages < 10; pages++ {
		items, next, err := r.s.ListAssuranceObservations(ctx, "t-1", "p-1", after, 2)
		require.NoError(t, err)
		for _, it := range items {
			seen = append(seen, it.ObservationID)
		}
		if next == "" {
			break
		}
		c, ok := store.ParseCursor(next)
		require.True(t, ok)
		after = &c
	}
	require.Len(t, seen, 5)
	uniq := map[string]bool{}
	for _, id := range seen {
		uniq[id] = true
	}
	assert.Len(t, uniq, 5, "no observation is returned twice")
}

func TestIdentity_GetSubjectIdentity(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	r.active(t, "t-1", "c-1", "https://idp.example.com")
	_, err := r.s.CreateLink(ctx, federation.Link{TenantID: "t-1", ConnectionID: "c-1", Subject: "u-1", PrincipalID: "p-1",
		Source: federation.SourceAdmin, LinkedBy: "admin"}, "admin", "corr")
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, r.s.CreateAuthSession(ctx, authSession("sid-live", "p-1", "t-1", now)))
	require.NoError(t, r.s.CreateAuthSession(ctx, authSession("sid-ended", "p-1", "t-1", now)))
	mustExec(t, r, `UPDATE auth_sessions SET ended_at = NOW(), end_reason = 'LOGOUT' WHERE sid = 'sid-ended'`)

	s, err := r.s.GetSubjectIdentity(ctx, "t-1", "p-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "p-1", s.PrincipalID)
	assert.Equal(t, 1, s.LinkCount)
	assert.Equal(t, 1, s.ActiveLinkCount)
	assert.Equal(t, 1, s.LiveSessions, "an ended session is not live")
	require.NotNil(t, s.CurrentAssurance)
	assert.Equal(t, "SINGLE_FACTOR", s.CurrentAssurance.AssuranceLevel)

	none, err := r.s.GetSubjectIdentity(ctx, "t-1", "p-2")
	require.NoError(t, err)
	require.NotNil(t, none)
	assert.Nil(t, none.CurrentAssurance, "a subject who never signed in has no assurance")
	assert.Zero(t, none.LinkCount)

	other, err := r.s.GetSubjectIdentity(ctx, "t-2", "p-1")
	require.NoError(t, err)
	assert.Nil(t, other, "a subject of another tenant is not found")
}

func TestIdentity_AuthenticationEventsReadModel(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	r.active(t, "t-1", "c-1", "https://idp.example.com")
	l, err := r.s.CreateLink(ctx, federation.Link{TenantID: "t-1", ConnectionID: "c-1", Subject: "u-1", PrincipalID: "p-1",
		Source: federation.SourceAdmin, LinkedBy: "admin"}, "admin", "corr")
	require.NoError(t, err)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	log := func(id, principal, tenant, action, outcome, basis string, at time.Time) {
		mustExec(t, r, `INSERT INTO access_decision_log (decision_log_id, principal_id, tenant_id, action_type,
			decision_outcome, decision_basis, correlation_id, decided_at) VALUES ($1,$2,$3,$4,$5,$6,'corr',$7)`,
			id, principal, tenant, action, outcome, basis, at)
	}
	log("d-1", "p-1", "t-1", "AUTHENTICATE", "DENIED", "bad_password", base)
	log("d-2", "p-1", "t-1", "STATUS_TRANSITION", "APPLIED", "SUSPENDED:admin", base.Add(time.Minute))
	log("d-3", "p-1", "t-1", "MFA_STEP_UP", "ALLOWED", "totp_verified", base.Add(2*time.Minute))
	log("d-other", "p-2", "t-1", "AUTHENTICATE", "ALLOWED", "password_verified", base.Add(3*time.Minute))
	require.NoError(t, r.s.CompleteLogin(ctx, federation.LoginRecord{TenantID: "t-1", ConnectionID: "c-1", ConfigVersion: 1,
		Issuer: "https://idp.example.com", Subject: "u-1", PrincipalID: "p-1", Outcome: federation.OutcomeSucceeded,
		Reason: "login", KeyID: "k1", CorrelationID: "corr-fed", MFAAsserted: true}, l.LinkID, "hash-1", time.Now().Add(5*time.Minute)))
	require.NoError(t, r.s.CreateAuthSession(ctx, authSession("sid-1", "p-1", "t-1", base)))
	mustExec(t, r, `UPDATE auth_sessions SET ended_at = NOW() + interval '1 second', end_reason = 'ADMIN_REVOKE' WHERE sid = 'sid-1'`)

	all, next, err := r.s.ListAuthenticationEvents(ctx, "t-1", "p-1", store.AuthenticationEventFilter{}, nil, 50)
	require.NoError(t, err)
	assert.Empty(t, next)
	types := map[string]int{}
	for _, e := range all {
		types[e.EventType]++
		assert.NotEqual(t, "d-other", e.EventID, "another subject's events are not mine")
		assert.NotEqual(t, "d-2", e.EventID, "a status change is not an authentication event")
	}
	assert.Equal(t, map[string]int{"AUTHENTICATE": 1, "MFA_STEP_UP": 1, "FEDERATED_LOGIN": 1, "SESSION_ENDED": 1}, types)
	// newest first: the session ended after everything else
	assert.Equal(t, "SESSION_ENDED", all[0].EventType)
	assert.Equal(t, "sid-1", all[0].SessionID)
	assert.Equal(t, "ADMIN_REVOKE", all[0].Reason)
	for i := 1; i < len(all); i++ {
		assert.False(t, all[i].OccurredAt.After(all[i-1].OccurredAt), "ordered newest first")
	}
	var fed store.AuthenticationEvent
	for _, e := range all {
		if e.EventType == "FEDERATED_LOGIN" {
			fed = e
		}
	}
	assert.Equal(t, "https://idp.example.com", fed.Issuer)
	assert.Equal(t, "c-1", fed.ConnectionID)
	require.NotNil(t, fed.MFAAsserted)
	assert.True(t, *fed.MFAAsserted)
	assert.Equal(t, "corr-fed", fed.CorrelationID)

	denied, _, err := r.s.ListAuthenticationEvents(ctx, "t-1", "p-1", store.AuthenticationEventFilter{Outcome: "DENIED"}, nil, 50)
	require.NoError(t, err)
	require.Len(t, denied, 1)
	assert.Equal(t, "d-1", denied[0].EventID)

	since := base.Add(time.Minute + time.Second)
	recent, _, err := r.s.ListAuthenticationEvents(ctx, "t-1", "p-1", store.AuthenticationEventFilter{Since: &since}, nil, 50)
	require.NoError(t, err)
	for _, e := range recent {
		assert.False(t, e.OccurredAt.Before(since))
	}
	assert.Len(t, recent, 3)

	var seen []string
	var after *store.Cursor
	for pages := 0; pages < 10; pages++ {
		items, nxt, err := r.s.ListAuthenticationEvents(ctx, "t-1", "p-1", store.AuthenticationEventFilter{}, after, 1)
		require.NoError(t, err)
		for _, e := range items {
			seen = append(seen, e.EventID)
		}
		if nxt == "" {
			break
		}
		c, ok := store.ParseCursor(nxt)
		require.True(t, ok)
		after = &c
	}
	assert.Len(t, seen, 4, "paging one at a time returns each event once")

	foreign, _, err := r.s.ListAuthenticationEvents(ctx, "t-2", "p-1", store.AuthenticationEventFilter{}, nil, 50)
	require.NoError(t, err)
	assert.Empty(t, foreign, "another tenant sees nothing")
}

func TestIdentity_CursorRoundTripAndRefusal(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 0, 0, 123456789, time.UTC)
	c, ok := store.ParseCursor(store.Cursor{At: at, ID: "abc|def"}.Encode())
	require.True(t, ok)
	assert.True(t, c.At.Equal(at))
	assert.Equal(t, "abc|def", c.ID, "an id containing the separator survives")
	for _, bad := range []string{"", "nonsense", "|id", "2026-11-03T10:00:00Z|", "not-a-time|id"} {
		_, ok := store.ParseCursor(bad)
		assert.False(t, ok, bad)
	}
}
