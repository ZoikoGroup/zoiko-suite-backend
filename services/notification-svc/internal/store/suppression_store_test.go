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

// A recorded suppression is never weakened, never deleted, and lifted only on
// evidence with a second principal (§7.3, migration 000022). The upsert used to
// overwrite the reason, so an unsubscribe arriving after a hard bounce turned
// it into a marketing-only row and security mail resumed to a dead address.
func TestSuppressionStore_NeverWeakenedNeverDeleted(t *testing.T) {
	pool, admin := openTestPools(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-supp-governance"
	email := "dead@example.com"
	add := func(reason ledger.SuppressionReason) {
		t.Helper()
		require.NoError(t, s.AddSuppression(ctx, &ledger.EmailSuppression{
			TenantID: tenantID, RecipientEmail: email, Reason: reason, SourceStream: "ALL", CreatedAt: time.Now().UTC(),
		}))
	}
	reasons := func() []string {
		t.Helper()
		rows, err := admin.Query(ctx, `SELECT reason || CASE WHEN lifted_at IS NULL THEN '' ELSE ':lifted' END
			FROM email_suppressions WHERE tenant_id = $1 ORDER BY created_at`, tenantID)
		require.NoError(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var r string
			require.NoError(t, rows.Scan(&r))
			out = append(out, r)
		}
		return out
	}

	// 1. An unsubscribe is strengthened by a later hard bounce...
	add(ledger.SuppressionReasonUnsubscribe)
	add(ledger.SuppressionReasonHardBounce)
	assert.Equal(t, []string{"HARD_BOUNCE"}, reasons())

	// 2. ...and a later unsubscribe or complaint cannot weaken it back.
	add(ledger.SuppressionReasonUnsubscribe)
	add(ledger.SuppressionReasonComplaint)
	assert.Equal(t, []string{"HARD_BOUNCE"}, reasons())
	blocked, _, err := s.IsEmailSuppressed(ctx, tenantID, email, ledger.StreamCritical, ledger.ClassS0)
	require.NoError(t, err)
	assert.True(t, blocked, "security mail must stay blocked to a hard-bounced address")

	// 3. Rows are never deleted.
	_, err = admin.Exec(ctx, `DELETE FROM email_suppressions WHERE tenant_id = $1`, tenantID)
	require.Error(t, err, "DELETE must be refused by the database")

	// 4. A lift needs evidence, and for a hard bounce a second principal.
	_, err = admin.Exec(ctx, `UPDATE email_suppressions SET lifted_at = now(), lifted_by_principal_id = 'op'
		WHERE tenant_id = $1`, tenantID)
	require.Error(t, err, "a lift without evidence must be refused")
	_, err = admin.Exec(ctx, `UPDATE email_suppressions SET lifted_at = now(), lifted_by_principal_id = 'op',
		lift_evidence_ref = 'ticket-1' WHERE tenant_id = $1`, tenantID)
	require.Error(t, err, "a hard-bounce lift without a second principal must be refused")
	_, err = admin.Exec(ctx, `UPDATE email_suppressions SET lifted_at = now(), lifted_by_principal_id = 'op',
		lift_evidence_ref = 'ticket-1', lift_approved_by_principal_id = 'op' WHERE tenant_id = $1`, tenantID)
	require.Error(t, err, "the approver must not be the lifter")
	_, err = admin.Exec(ctx, `UPDATE email_suppressions SET lifted_at = now(), lifted_by_principal_id = 'op',
		lift_evidence_ref = 'ticket-1', lift_approved_by_principal_id = 'checker' WHERE tenant_id = $1`, tenantID)
	require.NoError(t, err)

	blocked, _, err = s.IsEmailSuppressed(ctx, tenantID, email, ledger.StreamCritical, ledger.ClassS0)
	require.NoError(t, err)
	assert.False(t, blocked, "a lifted suppression no longer blocks")

	// 5. A lifted row is history: it cannot change again, and a fresh bounce
	// inserts a new active row beside it.
	_, err = admin.Exec(ctx, `UPDATE email_suppressions SET lift_evidence_ref = 'rewritten' WHERE tenant_id = $1`, tenantID)
	require.Error(t, err, "a lifted row must not change")
	add(ledger.SuppressionReasonHardBounce)
	assert.Equal(t, []string{"HARD_BOUNCE:lifted", "HARD_BOUNCE"}, reasons())
	blocked, _, err = s.IsEmailSuppressed(ctx, tenantID, email, ledger.StreamCritical, ledger.ClassS0)
	require.NoError(t, err)
	assert.True(t, blocked)
}
