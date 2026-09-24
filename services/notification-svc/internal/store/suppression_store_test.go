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

func TestSuppressionStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantA := "tenant-supp-a"
	tenantB := "tenant-supp-b"
	email := "user@example.com"

	// 1. Tenant A adds an unsubscribe suppression
	err := s.AddSuppression(ctx, &ledger.EmailSuppression{
		SuppressionID:  uuid.NewString(),
		TenantID:       tenantA,
		RecipientEmail: email,
		Reason:         ledger.SuppressionReasonUnsubscribe,
		SourceStream:   "ALL",
		CreatedAt:      time.Now().UTC(),
	})
	require.NoError(t, err)

	// 2. Tenant A checks suppression for M1 marketing -> suppressed
	suppressedA, reasonA, err := s.IsEmailSuppressed(ctx, tenantA, email, ledger.StreamMarketing, ledger.ClassM1)
	require.NoError(t, err)
	assert.True(t, suppressedA, "Tenant A must see the email as suppressed for M1")
	assert.Equal(t, string(ledger.SuppressionReasonUnsubscribe), reasonA)

	// 3. Tenant B checks same email -> NOT suppressed (RLS isolation)
	suppressedB, _, err := s.IsEmailSuppressed(ctx, tenantB, email, ledger.StreamMarketing, ledger.ClassM1)
	require.NoError(t, err)
	assert.False(t, suppressedB, "Tenant B must not see Tenant A's suppression")

	// 4. Tenant B lists suppressions -> empty
	listB, err := s.ListSuppressions(ctx, tenantB, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, listB, "Tenant B suppression list must be empty")
}

func TestSuppressionStore_Precedence_S0_T0_IgnoreUnsubscribe(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-precedence-1"
	email := "transact@example.com"

	// Add UNSUBSCRIBE suppression
	err := s.AddSuppression(ctx, &ledger.EmailSuppression{
		SuppressionID:  uuid.NewString(),
		TenantID:       tenantID,
		RecipientEmail: email,
		Reason:         ledger.SuppressionReasonUnsubscribe,
		SourceStream:   "ALL",
		CreatedAt:      time.Now().UTC(),
	})
	require.NoError(t, err)

	// S0 (Security) must NOT be suppressed by unsubscribe
	suppS0, _, err := s.IsEmailSuppressed(ctx, tenantID, email, ledger.StreamCritical, ledger.ClassS0)
	require.NoError(t, err)
	assert.False(t, suppS0, "S0 must bypass unsubscribe")

	// T0 (Transactional) must NOT be suppressed by unsubscribe
	suppT0, _, err := s.IsEmailSuppressed(ctx, tenantID, email, ledger.StreamTransactional, ledger.ClassT0)
	require.NoError(t, err)
	assert.False(t, suppT0, "T0 must bypass unsubscribe")

	// A1 (Operational) MUST be suppressed
	suppA1, _, err := s.IsEmailSuppressed(ctx, tenantID, email, ledger.StreamOperational, ledger.ClassA1)
	require.NoError(t, err)
	assert.True(t, suppA1, "A1 must be suppressed by unsubscribe")

	// M1 (Marketing) MUST be suppressed
	suppM1, _, err := s.IsEmailSuppressed(ctx, tenantID, email, ledger.StreamMarketing, ledger.ClassM1)
	require.NoError(t, err)
	assert.True(t, suppM1, "M1 must be suppressed by unsubscribe")

	// Now add HARD_BOUNCE suppression
	err = s.AddSuppression(ctx, &ledger.EmailSuppression{
		SuppressionID:  uuid.NewString(),
		TenantID:       tenantID,
		RecipientEmail: email,
		Reason:         ledger.SuppressionReasonHardBounce,
		SourceStream:   "ALL",
		CreatedAt:      time.Now().UTC(),
	})
	require.NoError(t, err)

	// S0 and T0 MUST be suppressed by HARD_BOUNCE (dead mailbox)
	suppS0Bounce, reasonS0, err := s.IsEmailSuppressed(ctx, tenantID, email, ledger.StreamCritical, ledger.ClassS0)
	require.NoError(t, err)
	assert.True(t, suppS0Bounce, "S0 must be blocked by HARD_BOUNCE")
	assert.Equal(t, string(ledger.SuppressionReasonHardBounce), reasonS0)
}

func TestSuppressionStore_RemoveAndUpsert(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-upsert-1"
	email := "flip@example.com"

	// 1. Add complaint
	err := s.AddSuppression(ctx, &ledger.EmailSuppression{
		SuppressionID:  uuid.NewString(),
		TenantID:       tenantID,
		RecipientEmail: email,
		Reason:         ledger.SuppressionReasonComplaint,
		SourceStream:   "MARKETING",
		CreatedAt:      time.Now().UTC(),
	})
	require.NoError(t, err)

	// 2. Remove suppression
	err = s.RemoveSuppression(ctx, tenantID, email, "MARKETING")
	require.NoError(t, err)

	supp, _, err := s.IsEmailSuppressed(ctx, tenantID, email, ledger.StreamMarketing, ledger.ClassM1)
	require.NoError(t, err)
	assert.False(t, supp, "Email must no longer be suppressed after removal")
}
