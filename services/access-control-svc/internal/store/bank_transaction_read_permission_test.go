//go:build integration

package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestBankTransactionReadPermissionRegistered proves migration 000022 made
// banking-connector-svc's BNK-04 read action grantable: until it is
// registered no bundle can name it and ListTransactions is always DENIED.
func TestBankTransactionReadPermissionRegistered(t *testing.T) {
	var naming, risk, description string
	err := ownerPool.QueryRow(context.Background(),
		`SELECT naming, risk_tier, description FROM permission_definitions WHERE action_name = 'bank.transaction.read'`).
		Scan(&naming, &risk, &description)
	require.NoError(t, err, "bank.transaction.read was not registered")
	require.Equal(t, "TAXONOMY", naming)
	require.Equal(t, "STANDARD", risk)
	require.Contains(t, description, "banking-connector-svc")
}

// TestBankTransactionReadPermissionDown proves both paths of 000022's down
// guard, each inside a transaction that is rolled back so the shared schema
// keeps the registration: refused while a bundle grants the action, and
// removing the row once none does.
func TestBankTransactionReadPermissionDown(t *testing.T) {
	downSQL, err := os.ReadFile("../../deployments/migrations/000022_register_bank_transaction_read.down.sql")
	require.NoError(t, err)
	ctx := context.Background()

	t.Run("refused while a bundle grants it", func(t *testing.T) {
		tx, err := ownerPool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		tenantID := "tenant-bank-txn-read-down-" + uuid.NewString()
		roleID := uuid.NewString()
		now := time.Now().UTC()
		_, err = tx.Exec(ctx, `
			INSERT INTO role_definitions (
				role_definition_id, tenant_id, role_code, role_name, role_scope_type, status,
				created_by_principal_id, correlation_id, created_at, updated_at
			) VALUES ($1, $2, 'BANK_RECONCILER', 'Bank reconciler', 'TENANT', 'ACTIVE', 'test', $3, $4, $4)`,
			roleID, tenantID, "corr-role-"+roleID, now)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `
			INSERT INTO permission_bundle_defs (
				bundle_id, tenant_id, role_definition_id, bundle_code, permitted_actions,
				active_flag, correlation_id, created_at, updated_at
			) VALUES ($1, $2, $3, 'BANK_RECONCILER', ARRAY['bank.transaction.read'], TRUE, $4, $5, $5)`,
			uuid.NewString(), tenantID, roleID, "corr-bundle-"+roleID, now)
		require.NoError(t, err)

		_, err = tx.Exec(ctx, string(downSQL))
		require.Error(t, err)
		require.Contains(t, err.Error(), "000022 down refused")
	})

	t.Run("removes the row when nothing grants it", func(t *testing.T) {
		tx, err := ownerPool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		_, err = tx.Exec(ctx, string(downSQL))
		require.NoError(t, err)
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM permission_definitions WHERE action_name = 'bank.transaction.read'`).Scan(&n))
		require.Zero(t, n)
	})
}
