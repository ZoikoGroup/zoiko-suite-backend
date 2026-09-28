package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/store"
)

func createTestIntent(t *testing.T, s *store.PgStore, tenantID, email string) *ledger.MessageIntent {
	t.Helper()
	intentID := uuid.NewString()
	intent := &ledger.MessageIntent{
		MessageIntentID:      intentID,
		TenantID:             tenantID,
		LegalEntityID:        "le-1",
		RecipientPrincipalID: "usr-" + uuid.NewString()[:8],
		RecipientEmail:       email,
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassT0,
		TemplateKey:          "ZS-IA-001",
		SourceEventType:      "test.event",
		DeduplicationKey:     "dedup-" + intentID,
		CorrelationID:        "corr-" + intentID,
		Status:               ledger.IntentStatusPending,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}
	_, _, err := s.CreateMessageIntent(context.Background(), intent)
	require.NoError(t, err)
	return intent
}

func TestActionTokenStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantA := "tenant-action-a"
	tenantB := "tenant-action-b"

	intentA := createTestIntent(t, s, tenantA, "alice@example.com")

	tokenHash := "hash-aaa-" + uuid.NewString()
	token := &ledger.ActionToken{
		TokenID:              uuid.NewString(),
		TokenHash:            tokenHash,
		MessageIntentID:      intentA.MessageIntentID,
		TenantID:             tenantA,
		RecipientPrincipalID: intentA.RecipientPrincipalID,
		Purpose:              "VERIFY_EMAIL",
		TargetActionURL:      "https://app.zoiko.com/verify",
		TargetMethod:         "POST",
		Payload:              json.RawMessage(`{"user_id":"u-123"}`),
		Status:               ledger.ActionTokenStatusActive,
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour),
		CreatedAt:            time.Now().UTC(),
	}

	err := s.CreateActionToken(ctx, token)
	require.NoError(t, err)

	// 1. Tenant A can read token
	gotA, err := s.GetActionTokenByHash(ctx, tenantA, tokenHash)
	require.NoError(t, err)
	assert.Equal(t, token.TokenID, gotA.TokenID)
	assert.Equal(t, "VERIFY_EMAIL", gotA.Purpose)
	assert.Equal(t, ledger.ActionTokenStatusActive, gotA.Status)

	// 2. Tenant B cannot read token (RLS isolation -> ErrActionTokenNotFound)
	gotB, err := s.GetActionTokenByHash(ctx, tenantB, tokenHash)
	assert.ErrorIs(t, err, store.ErrActionTokenNotFound)
	assert.Nil(t, gotB)

	// 3. Tenant B cannot consume Tenant A's token
	consumedB, err := s.ConsumeActionToken(ctx, tenantB, tokenHash, "192.168.1.10")
	assert.ErrorIs(t, err, store.ErrActionTokenNotFound)
	assert.Nil(t, consumedB)
}

func TestActionTokenStore_SingleUse_Consumption(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-action-consume"
	intent := createTestIntent(t, s, tenantID, "bob@example.com")

	tokenHash := "hash-consume-" + uuid.NewString()
	token := &ledger.ActionToken{
		TokenID:              uuid.NewString(),
		TokenHash:            tokenHash,
		MessageIntentID:      intent.MessageIntentID,
		TenantID:             tenantID,
		RecipientPrincipalID: intent.RecipientPrincipalID,
		Purpose:              "RESET_PASSWORD",
		TargetActionURL:      "https://app.zoiko.com/auth/reset",
		TargetMethod:         "POST",
		Payload:              json.RawMessage(`{"role":"admin"}`),
		Status:               ledger.ActionTokenStatusActive,
		ExpiresAt:            time.Now().UTC().Add(30 * time.Minute),
		CreatedAt:            time.Now().UTC(),
	}

	err := s.CreateActionToken(ctx, token)
	require.NoError(t, err)

	// First consumption -> SUCCESS
	clientIP := "203.0.113.195"
	consumed, err := s.ConsumeActionToken(ctx, tenantID, tokenHash, clientIP)
	require.NoError(t, err)
	assert.Equal(t, ledger.ActionTokenStatusConsumed, consumed.Status)
	assert.NotNil(t, consumed.ConsumedAt)
	assert.NotNil(t, consumed.ConsumedByIP)
	assert.Equal(t, clientIP, *consumed.ConsumedByIP)

	// Second consumption -> FAILS with ErrActionTokenAlreadyConsumed (single-use invariant)
	consumedAgain, err := s.ConsumeActionToken(ctx, tenantID, tokenHash, clientIP)
	assert.ErrorIs(t, err, store.ErrActionTokenAlreadyConsumed)
	assert.Nil(t, consumedAgain)
}

func TestActionTokenStore_ExpiredToken(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-action-expired"
	intent := createTestIntent(t, s, tenantID, "charlie@example.com")

	tokenHash := "hash-expired-" + uuid.NewString()
	token := &ledger.ActionToken{
		TokenID:              uuid.NewString(),
		TokenHash:            tokenHash,
		MessageIntentID:      intent.MessageIntentID,
		TenantID:             tenantID,
		RecipientPrincipalID: intent.RecipientPrincipalID,
		Purpose:              "ONE_TIME_SIGN_IN",
		TargetActionURL:      "https://app.zoiko.com/auth/verify",
		TargetMethod:         "POST",
		Status:               ledger.ActionTokenStatusActive,
		ExpiresAt:            time.Now().UTC().Add(-10 * time.Minute), // Expired!
		CreatedAt:            time.Now().UTC().Add(-30 * time.Minute),
	}

	err := s.CreateActionToken(ctx, token)
	require.NoError(t, err)

	consumed, err := s.ConsumeActionToken(ctx, tenantID, tokenHash, "10.0.0.1")
	assert.ErrorIs(t, err, store.ErrActionTokenExpired)
	assert.Nil(t, consumed)
}

func TestActionTokenStore_RevokeToken(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-action-revoke"
	intent := createTestIntent(t, s, tenantID, "dave@example.com")

	tokenID := uuid.NewString()
	tokenHash := "hash-revoked-" + uuid.NewString()
	token := &ledger.ActionToken{
		TokenID:              tokenID,
		TokenHash:            tokenHash,
		MessageIntentID:      intent.MessageIntentID,
		TenantID:             tenantID,
		RecipientPrincipalID: intent.RecipientPrincipalID,
		Purpose:              "WORKSPACE_INVITE",
		TargetActionURL:      "https://app.zoiko.com/workspaces/join",
		TargetMethod:         "POST",
		Status:               ledger.ActionTokenStatusActive,
		ExpiresAt:            time.Now().UTC().Add(24 * time.Hour),
		CreatedAt:            time.Now().UTC(),
	}

	err := s.CreateActionToken(ctx, token)
	require.NoError(t, err)

	// Revoke token
	err = s.RevokeActionToken(ctx, tenantID, tokenID)
	require.NoError(t, err)

	// Consume should fail with ErrActionTokenRevoked
	consumed, err := s.ConsumeActionToken(ctx, tenantID, tokenHash, "10.0.0.1")
	assert.ErrorIs(t, err, store.ErrActionTokenRevoked)
	assert.Nil(t, consumed)
}
