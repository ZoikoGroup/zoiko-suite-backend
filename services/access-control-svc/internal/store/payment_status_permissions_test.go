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

// TestPaymentStatusPermissionDefinitionsRegistered proves migration 000020
// actually made the BNK-07 actions grantable. Before it, every payment-status
// authorization check was permanently DENIED because no bundle could name an
// unregistered action.
func TestPaymentStatusPermissionDefinitionsRegistered(t *testing.T) {
	ctx := context.Background()
	want := map[string]string{
		"PAYMENT_STATUS_READ":      "STANDARD",
		"PAYMENT_STATUS_INGEST":    "STANDARD",
		"PAYMENT_STATUS_RESOLVE":   "STANDARD",
		"PAYMENT_FINALITY_CONFIRM": "HIGH",
	}
	for action, risk := range want {
		var gotRisk, description string
		err := ownerPool.QueryRow(ctx,
			`SELECT risk_tier, description FROM permission_definitions WHERE action_name = $1`,
			action).Scan(&gotRisk, &description)
		require.NoError(t, err, "action %s was not registered", action)
		require.Equal(t, risk, gotRisk, "action %s risk tier", action)
		require.Contains(t, description, "payment-status-svc", "action %s description", action)
	}
}

// TestPaymentStatusPermissionDownRefusesWhileBundleUsesAction proves the
// migration's down guard. It runs inside a transaction that is rolled back, so
// the shared test schema keeps the registered actions.
func TestPaymentStatusPermissionDownRefusesWhileBundleUsesAction(t *testing.T) {
	downSQL, err := os.ReadFile("../../deployments/migrations/000020_register_payment_status_actions.down.sql")
	require.NoError(t, err)

	ctx := context.Background()
	tx, err := ownerPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	tenantID := "tenant-payment-status-down-" + uuid.NewString()
	roleID := uuid.NewString()
	_, err = tx.Exec(ctx, `
		INSERT INTO role_definitions (
			role_definition_id, tenant_id, role_code, role_name, role_scope_type, status,
			created_by_principal_id, correlation_id, created_at, updated_at
		) VALUES ($1, $2, 'PAYMENT_OPERATOR', 'Payment operator', 'TENANT', 'ACTIVE',
		          'test', $3, $4, $4)`,
		roleID, tenantID, "corr-role-"+roleID, time.Now().UTC())
	require.NoError(t, err)

	_, err = tx.Exec(ctx, `
		INSERT INTO permission_bundle_defs (
			bundle_id, tenant_id, role_definition_id, bundle_code, permitted_actions,
			active_flag, correlation_id, created_at, updated_at
		) VALUES ($1, $2, $3, 'PAYMENT_OPERATOR', ARRAY['PAYMENT_FINALITY_CONFIRM'],
		          TRUE, $4, $5, $5)`,
		uuid.NewString(), tenantID, roleID, "corr-bundle-"+roleID, time.Now().UTC())
	require.NoError(t, err)

	_, err = tx.Exec(ctx, string(downSQL))
	require.Error(t, err)
	require.Contains(t, err.Error(), "000020 down refused")
}
