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

func strp(s string) *string { return &s }

func TestIntentVersion_PrivacyBindingIsValidatedStoredAndFrozen(t *testing.T) {
	s := store.New(openAdminTestPool(t))
	tenant := "tenant-privacy"
	i := newIntent(t, s, tenant, "payroll.payslip_available")
	ctx := tenantCtx(tenant)

	// Both or neither; shaped like an identifier.
	for _, bad := range []func(*domain.CreateIntentVersionParams){
		func(p *domain.CreateIntentVersionParams) { p.PrivacyActivityID = strp("act-1") },
		func(p *domain.CreateIntentVersionParams) { p.PrivacyPurposeID = strp("pur-1") },
		func(p *domain.CreateIntentVersionParams) {
			p.PrivacyActivityID, p.PrivacyPurposeID = strp("a b"), strp("pur-1")
		},
		func(p *domain.CreateIntentVersionParams) {
			p.PrivacyActivityID, p.PrivacyPurposeID = strp("act-1"), strp("")
		},
	} {
		p := domain.CreateIntentVersionParams{IntentID: i.IntentID, PurposeClass: "T0", EvidenceClass: "E2",
			AllowedChannels: []string{"EMAIL"}, VariableContract: payslipContract(), CreatedByPrincipalID: "maker"}
		bad(&p)
		_, err := s.CreateIntentVersion(ctx, p)
		var ip domain.IntentProblem
		assert.ErrorAs(t, err, &ip)
	}

	v := draftVersion(t, s, tenant, i.IntentID, func(p *domain.CreateIntentVersionParams) {
		p.PrivacyActivityID, p.PrivacyPurposeID = strp("act-payslip"), strp("pur-employment")
	})
	require.NotNil(t, v.PrivacyActivityID)
	assert.Equal(t, "act-payslip", *v.PrivacyActivityID)
	assert.Equal(t, "pur-employment", *v.PrivacyPurposeID)
	got, err := s.GetIntentVersion(ctx, v.VersionID)
	require.NoError(t, err)
	assert.Equal(t, "act-payslip", *got.PrivacyActivityID, "read back from the store")

	// An unbound version reads back as unbound.
	u := draftVersion(t, s, tenant, i.IntentID, nil)
	assert.Nil(t, u.PrivacyActivityID)
	assert.Nil(t, u.PrivacyPurposeID)
}

func TestIntentVersion_PrivacyBindingCannotBeEditedInTheDatabase(t *testing.T) {
	pool := openAdminTestPool(t)
	s := store.New(openAdminTestPool(t))
	tenant := "tenant-privacy-frozen"
	i := newIntent(t, s, tenant, "payroll.payslip_available")
	v := draftVersion(t, s, tenant, i.IntentID, func(p *domain.CreateIntentVersionParams) {
		p.PrivacyActivityID, p.PrivacyPurposeID = strp("act-1"), strp("pur-1")
	})
	_, err := pool.Exec(context.Background(), `UPDATE communication_intent_versions SET privacy_purpose_id = 'pur-2' WHERE version_id = $1::uuid`, v.VersionID)
	require.Error(t, err, "the binding is part of the frozen content")
	assert.Contains(t, err.Error(), "immutable")

	_, err = pool.Exec(context.Background(), `UPDATE communication_intent_versions SET privacy_activity_id = NULL WHERE version_id = $1::uuid`, v.VersionID)
	require.Error(t, err, "dropping half the binding is refused too (immutable or the both-or-neither check)")
}

func TestAttempts_RecordThePrivacyDecisionThatGovernedThem(t *testing.T) {
	s := store.New(openAdminTestPool(t))
	ctx := tenantCtx("tenant-priv-att")
	n := seedNotification(t, s, "tenant-priv-att", "corr-priv-att")

	at := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, s.ScheduleRetry(ctx, n.NotificationID, "tenant-priv-att", "NCD-008 down", at, time.Now().UTC(),
		domain.AttemptMeta{Origin: domain.AttemptOriginRequest, Retryable: true, PrivacyResult: "UNAVAILABLE"}))
	done := time.Now().UTC()
	require.NoError(t, s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "250 ok", &done, "c",
		domain.AttemptMeta{Origin: domain.AttemptOriginRetry, ProviderName: "smtp", PrivacyDecisionID: "d-77", PrivacyResult: "PERMIT"}))

	got, err := s.ListAttempts(ctx, n.NotificationID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Empty(t, got[0].PrivacyDecisionID, "a refusal with no decision records none")
	assert.Equal(t, "UNAVAILABLE", got[0].PrivacyResult)
	assert.Equal(t, "d-77", got[1].PrivacyDecisionID)
	assert.Equal(t, "PERMIT", got[1].PrivacyResult)

	// An attempt that never consulted privacy records nothing, exactly as before.
	m := seedNotification(t, s, "tenant-priv-att", "corr-priv-att-2")
	require.NoError(t, s.CompleteDelivery(ctx, m.NotificationID, "SENT", "", "250 ok", &done, "c", domain.AttemptMeta{Origin: domain.AttemptOriginRequest}))
	plain, err := s.ListAttempts(ctx, m.NotificationID)
	require.NoError(t, err)
	require.Len(t, plain, 1)
	assert.Empty(t, plain[0].PrivacyResult)
}

func TestAttempts_PrivacyResultMustBeAKnownValue(t *testing.T) {
	s := store.New(openAdminTestPool(t))
	n := seedNotification(t, s, "tenant-priv-bad", "corr-priv-bad")
	done := time.Now().UTC()
	err := s.CompleteDelivery(tenantCtx("tenant-priv-bad"), n.NotificationID, "SENT", "", "250 ok", &done, "c",
		domain.AttemptMeta{Origin: domain.AttemptOriginRequest, PrivacyResult: "WHATEVER"})
	require.Error(t, err, "an unknown privacy result is not recordable")
}

func TestMigration000022_DownThenUp(t *testing.T) {
	pool := openAdminTestPool(t)
	for _, f := range []string{"000022_privacy_binding_and_evidence.down.sql", "000022_privacy_binding_and_evidence.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), string(b))
		require.NoError(t, err, f)
	}
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.columns
		WHERE (table_name='communication_intent_versions' AND column_name IN ('privacy_activity_id','privacy_purpose_id'))
		   OR (table_name='notification_delivery_attempts' AND column_name IN ('privacy_decision_id','privacy_result'))`).Scan(&n))
	assert.Equal(t, 4, n)
}
