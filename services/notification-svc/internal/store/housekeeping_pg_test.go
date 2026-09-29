package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/store"
)

func TestHousekeepingStore_ActionToken_Expiry_And_Purge(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-hk-tokens"
	now := time.Now().UTC()

	// Seed Intent for Foreign Key
	intentID := uuid.NewString()
	_, _, err := s.CreateMessageIntent(ctx, &ledger.MessageIntent{
		MessageIntentID:      intentID,
		TenantID:             tenantID,
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "principal-001",
		RecipientEmail:       "principal@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassT0,
		TemplateKey:          "ZS-IA-001",
		SourceEventType:      "test.event",
		DeduplicationKey:     "dedup-hk-tok-1",
		CorrelationID:        "corr-hk-tok-1",
		Status:               ledger.IntentStatusDispatched,
		CreatedAt:            now,
		UpdatedAt:            now,
	})
	require.NoError(t, err)

	// 1. Token A: ACTIVE but expired 1 hour ago
	tokenA := &ledger.ActionToken{
		TokenID:              uuid.NewString(),
		TokenHash:            "hash-tok-expired",
		MessageIntentID:      intentID,
		TenantID:             tenantID,
		RecipientPrincipalID: "principal-001",
		Purpose:              "password_reset",
		TargetActionURL:      "https://auth.zoiko.com/reset",
		TargetMethod:         "POST",
		Status:               ledger.ActionTokenStatusActive,
		ExpiresAt:            now.Add(-1 * time.Hour),
		CreatedAt:            now.Add(-2 * time.Hour),
	}
	err = s.CreateActionToken(ctx, tokenA)
	require.NoError(t, err)

	// 2. Token B: ACTIVE and expires in 1 hour (must NOT expire)
	tokenB := &ledger.ActionToken{
		TokenID:              uuid.NewString(),
		TokenHash:            "hash-tok-future",
		MessageIntentID:      intentID,
		TenantID:             tenantID,
		RecipientPrincipalID: "principal-001",
		Purpose:              "email_verify",
		TargetActionURL:      "https://auth.zoiko.com/verify",
		TargetMethod:         "POST",
		Status:               ledger.ActionTokenStatusActive,
		ExpiresAt:            now.Add(1 * time.Hour),
		CreatedAt:            now,
	}
	err = s.CreateActionToken(ctx, tokenB)
	require.NoError(t, err)

	// 3. Token C: CONSUMED created 40 days ago (older than 30d cutoff)
	tokenC := &ledger.ActionToken{
		TokenID:              uuid.NewString(),
		TokenHash:            "hash-tok-old-consumed",
		MessageIntentID:      intentID,
		TenantID:             tenantID,
		RecipientPrincipalID: "principal-001",
		Purpose:              "sign_in",
		TargetActionURL:      "https://auth.zoiko.com/signin",
		TargetMethod:         "POST",
		Status:               ledger.ActionTokenStatusConsumed,
		ExpiresAt:            now.Add(-39 * 24 * time.Hour),
		CreatedAt:            now.Add(-40 * 24 * time.Hour),
	}
	err = s.CreateActionToken(ctx, tokenC)
	require.NoError(t, err)

	// Execution: Discovery & Expiry
	tenants, err := s.FindTenantsWithExpiredActionTokens(ctx, now, 50)
	require.NoError(t, err)
	assert.Contains(t, tenants, tenantID)

	expiredCount, err := s.ExpireActionTokensForTenant(ctx, tenantID, now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), expiredCount, "Only Token A should have been transitioned to EXPIRED")

	// Verify Token A is now EXPIRED
	fetchedA, err := s.GetActionTokenByHash(ctx, tenantID, tokenA.TokenHash)
	require.NoError(t, err)
	assert.Equal(t, ledger.ActionTokenStatusExpired, fetchedA.Status)

	// Verify Token B is still ACTIVE
	fetchedB, err := s.GetActionTokenByHash(ctx, tenantID, tokenB.TokenHash)
	require.NoError(t, err)
	assert.Equal(t, ledger.ActionTokenStatusActive, fetchedB.Status)

	// Execution: Purge terminal tokens older than 30 days
	cutoff30d := now.Add(-30 * 24 * time.Hour)
	purgeTenants, err := s.FindTenantsWithPurgeableActionTokens(ctx, cutoff30d, 50)
	require.NoError(t, err)
	assert.Contains(t, purgeTenants, tenantID)

	purgedCount, err := s.PurgeActionTokensForTenant(ctx, tenantID, cutoff30d)
	require.NoError(t, err)
	assert.Equal(t, int64(1), purgedCount, "Token C should have been purged")

	// Token C should no longer exist
	_, err = s.GetActionTokenByHash(ctx, tenantID, tokenC.TokenHash)
	assert.ErrorIs(t, err, store.ErrActionTokenNotFound)
}

func TestHousekeepingStore_StaleIntents_Expiry(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-hk-intents"
	now := time.Now().UTC()

	// Intent 1: PENDING created 30 hours ago (stale)
	staleIntentID := uuid.NewString()
	_, _, err := s.CreateMessageIntent(ctx, &ledger.MessageIntent{
		MessageIntentID:      staleIntentID,
		TenantID:             tenantID,
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "principal-001",
		RecipientEmail:       "stale@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassT0,
		TemplateKey:          "ZS-IA-001",
		SourceEventType:      "test.event",
		DeduplicationKey:     "dedup-stale-1",
		CorrelationID:        "corr-stale-1",
		Status:               ledger.IntentStatusPending,
		CreatedAt:            now.Add(-30 * time.Hour),
		UpdatedAt:            now.Add(-30 * time.Hour),
	})
	require.NoError(t, err)

	// Intent 2: PENDING created 1 hour ago (recent, must remain PENDING)
	recentIntentID := uuid.NewString()
	_, _, err = s.CreateMessageIntent(ctx, &ledger.MessageIntent{
		MessageIntentID:      recentIntentID,
		TenantID:             tenantID,
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "principal-001",
		RecipientEmail:       "recent@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassT0,
		TemplateKey:          "ZS-IA-001",
		SourceEventType:      "test.event",
		DeduplicationKey:     "dedup-recent-1",
		CorrelationID:        "corr-recent-1",
		Status:               ledger.IntentStatusPending,
		CreatedAt:            now.Add(-1 * time.Hour),
		UpdatedAt:            now.Add(-1 * time.Hour),
	})
	require.NoError(t, err)

	// Run stale intent expiry with 24h cutoff
	staleCutoff := now.Add(-24 * time.Hour)
	tenants, err := s.FindTenantsWithStaleIntents(ctx, staleCutoff, 50)
	require.NoError(t, err)
	assert.Contains(t, tenants, tenantID)

	count, err := s.ExpireStaleIntentsForTenant(ctx, tenantID, staleCutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	// Stale intent is now FAILED
	intent1, err := s.GetMessageIntent(ctx, tenantID, staleIntentID)
	require.NoError(t, err)
	assert.Equal(t, ledger.IntentStatusFailed, intent1.Status)
	assert.Contains(t, *intent1.FailureReason, "stale intent expired")

	// Recent intent remains PENDING
	intent2, err := s.GetMessageIntent(ctx, tenantID, recentIntentID)
	require.NoError(t, err)
	assert.Equal(t, ledger.IntentStatusPending, intent2.Status)
}

func TestHousekeepingStore_PurgeCompletedLedgerRecords(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-hk-ledger"
	now := time.Now().UTC()

	// Intent 1: DISPATCHED 100 days ago (older than 90d cutoff)
	oldIntentID := uuid.NewString()
	oldRenderID := uuid.NewString()
	oldAttemptID := uuid.NewString()

	_, _, err := s.CreateMessageIntent(ctx, &ledger.MessageIntent{
		MessageIntentID:      oldIntentID,
		TenantID:             tenantID,
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "principal-001",
		RecipientEmail:       "old@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassT0,
		TemplateKey:          "ZS-IA-001",
		SourceEventType:      "test.event",
		DeduplicationKey:     "dedup-old-ledger-1",
		CorrelationID:        "corr-old-ledger-1",
		Status:               ledger.IntentStatusDispatched,
		CreatedAt:            now.Add(-100 * 24 * time.Hour),
		UpdatedAt:            now.Add(-100 * 24 * time.Hour),
	})
	require.NoError(t, err)

	err = s.RecordMessageRender(ctx, &ledger.MessageRender{
		RenderID:        oldRenderID,
		MessageIntentID: oldIntentID,
		TenantID:        tenantID,
		ContentHash:     "hash-old",
		Subject:         "Old Notice",
		BodyHTML:        "<p>Old</p>",
		BodyText:        "Old",
		RenderedAt:      now.Add(-100 * 24 * time.Hour),
	})
	require.NoError(t, err)

	err = s.RecordDeliveryAttempt(ctx, &ledger.DeliveryAttempt{
		ProviderAttemptID: oldAttemptID,
		MessageIntentID:   oldIntentID,
		RenderID:          oldRenderID,
		TenantID:          tenantID,
		SenderStream:      ledger.StreamTransactional,
		FromAddress:       "notifications@notify.zoikosuite.com",
		ToAddress:         "old@example.com",
		ProviderName:      "smtp",
		Status:            ledger.AttemptStatusAccepted,
		AttemptNumber:     1,
		AttemptedAt:       now.Add(-100 * 24 * time.Hour),
	})
	require.NoError(t, err)

	// Intent 2: DISPATCHED 10 days ago (recent, must NOT be purged)
	recentIntentID := uuid.NewString()
	_, _, err = s.CreateMessageIntent(ctx, &ledger.MessageIntent{
		MessageIntentID:      recentIntentID,
		TenantID:             tenantID,
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "principal-001",
		RecipientEmail:       "recent@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassT0,
		TemplateKey:          "ZS-IA-001",
		SourceEventType:      "test.event",
		DeduplicationKey:     "dedup-recent-ledger-1",
		CorrelationID:        "corr-recent-ledger-1",
		Status:               ledger.IntentStatusDispatched,
		CreatedAt:            now.Add(-10 * 24 * time.Hour),
		UpdatedAt:            now.Add(-10 * 24 * time.Hour),
	})
	require.NoError(t, err)

	// Purge with 90-day cutoff
	cutoff90d := now.Add(-90 * 24 * time.Hour)
	tenants, err := s.FindTenantsWithCompletedIntents(ctx, cutoff90d, 50)
	require.NoError(t, err)
	assert.Contains(t, tenants, tenantID)

	purgedCount, err := s.PurgeCompletedLedgerRecordsForTenant(ctx, tenantID, cutoff90d)
	require.NoError(t, err)
	assert.Equal(t, int64(1), purgedCount, "Old intent should have been deleted")

	// Verify old intent is gone
	_, err = s.GetMessageIntent(ctx, tenantID, oldIntentID)
	assert.Error(t, err)

	// Verify recent intent still exists
	recentIntent, err := s.GetMessageIntent(ctx, tenantID, recentIntentID)
	require.NoError(t, err)
	assert.Equal(t, ledger.IntentStatusDispatched, recentIntent.Status)
}
