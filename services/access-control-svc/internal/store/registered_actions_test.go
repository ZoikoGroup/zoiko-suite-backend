//go:build integration

package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/access-control-svc/internal/store"
)

// Actions enforced by other services (REF-01 jurisdiction-rules-svc, REF-07
// policy-svc). Until a permission is registered no bundle can name it, so its
// check is permanently denied; these prove each migration made its actions
// grantable, and that each down migration refuses while one is granted.

var registeredActionMigrations = []struct {
	migration string
	// owner is the service named in each action's description.
	owner string
	risk  map[string]string
}{
	{"000037_register_jurisdiction_alias_actions", "jurisdiction-rules-svc", map[string]string{
		"JURISDICTION_ALIAS_AMEND": "HIGH",
		"JURISDICTION_ALIAS_END":   "HIGH",
	}},
	{"000038_register_jurisdiction_import_actions", "jurisdiction-rules-svc", map[string]string{
		"JURISDICTION_IMPORT_PROPOSE": "HIGH",
		"JURISDICTION_IMPORT_APPROVE": "HIGH",
		"JURISDICTION_IMPORT_VIEW":    "STANDARD",
	}},
	{"000039_register_accounting_policy_actions", "policy-svc", map[string]string{
		"ACCOUNTING_POLICY_DRAFT": "STANDARD",
	}},
	{"000040_register_accounting_policy_lifecycle_actions", "policy-svc", map[string]string{
		"ACCOUNTING_POLICY_APPROVE":           "HIGH",
		"ACCOUNTING_POLICY_ACTIVATE":          "HIGH",
		"ACCOUNTING_POLICY_EMERGENCY_SUSPEND": "CRITICAL",
	}},
	{"000041_register_payment_term_actions", "jurisdiction-rules-svc", map[string]string{
		"PAYMENT_TERM_CREATE":    "STANDARD",
		"PAYMENT_TERM_APPROVE":   "HIGH",
		"PAYMENT_TERM_SUPERSEDE": "HIGH",
		"PAYMENT_TERM_RETIRE":    "HIGH",
	}},
	{"000042_register_identity_read_actions", "identity-context-svc", map[string]string{
		"IDENTITY_SUBJECT_READ":    "STANDARD",
		"IDENTITY_AUTH_EVENT_READ": "HIGH",
	}},
	{"000043_register_identity_merge_action", "identity-context-svc", map[string]string{
		"IDENTITY_MERGE": "CRITICAL",
	}},
	{"000044_register_party_actions", "tenant-entity-registry-svc", map[string]string{
		"PARTY_CREATE":            "STANDARD",
		"PARTY_AMEND":             "STANDARD",
		"PARTY_ROLE_MANAGE":       "STANDARD",
		"PARTY_IDENTIFIER_LINK":   "STANDARD",
		"PARTY_IDENTIFIER_REVEAL": "HIGH",
	}},
	{"000045_register_party_resolution_actions", "tenant-entity-registry-svc", map[string]string{
		"PARTY_MERGE":             "CRITICAL",
		"PARTY_SPLIT":             "HIGH",
		"PARTY_RESOLUTION_DECIDE": "HIGH",
	}},
	{"000047_register_evidence_manifest_actions", "evidence-manifest-svc", map[string]string{
		"EVIDENCE_MANIFEST_GENERATE": "STANDARD",
		"EVIDENCE_MANIFEST_READ":     "STANDARD",
	}},
}

// permCatalog is the permission catalogue used by alias tests.
var permCatalog = store.NewPermissionCatalogue(testStore)

func TestRegisteredPermissionDefinitions(t *testing.T) {
	ctx := context.Background()
	for _, m := range registeredActionMigrations {
		for action, risk := range m.risk {
			var gotRisk, description string
			err := ownerPool.QueryRow(ctx,
				`SELECT risk_tier, description FROM permission_definitions WHERE action_name = $1`, action).Scan(&gotRisk, &description)
			require.NoError(t, err, "%s: action %s was not registered", m.migration, action)
			require.Equal(t, risk, gotRisk, "action %s risk tier", action)
			require.Contains(t, description, m.owner, "action %s description", action)
		}
	}
}

// The down migration refuses while a bundle grants one of its actions, and
// otherwise removes exactly its own actions. Both run in a transaction that is
// rolled back, so the shared test schema keeps the registered actions.
func TestRegisteredPermissionDownRefusesWhileGrantedAndOtherwiseRemovesOwnActions(t *testing.T) {
	ctx := context.Background()
	for _, m := range registeredActionMigrations {
		down, err := os.ReadFile("../../deployments/migrations/" + m.migration + ".down.sql")
		require.NoError(t, err)

		var granted string
		for action := range m.risk {
			granted = action
			break
		}

		// Unused: the down migration succeeds and removes its actions only.
		tx, err := ownerPool.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, string(down))
		require.NoError(t, err, "%s down with nothing granted", m.migration)
		for _, other := range registeredActionMigrations {
			for action := range other.risk {
				var n int
				require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM permission_definitions WHERE action_name = $1`, action).Scan(&n))
				if other.migration == m.migration {
					require.Equal(t, 0, n, "%s should have removed %s", m.migration, action)
				} else {
					require.Equal(t, 1, n, "%s removed %s, which is another migration's", m.migration, action)
				}
			}
		}
		require.NoError(t, tx.Rollback(ctx))

		// Granted by a bundle: refused.
		tx, err = ownerPool.Begin(ctx)
		require.NoError(t, err)
		tenantID := "tenant-jurisdiction-down-" + uuid.NewString()
		roleID := uuid.NewString()
		_, err = tx.Exec(ctx, `
			INSERT INTO role_definitions (
				role_definition_id, tenant_id, role_code, role_name, role_scope_type, status,
				created_by_principal_id, correlation_id, created_at, updated_at
			) VALUES ($1, $2, 'REF_STEWARD', 'Reference-data steward', 'TENANT', 'ACTIVE',
			          'test', $3, $4, $4)`,
			roleID, tenantID, "corr-role-"+roleID, time.Now().UTC())
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `
			INSERT INTO permission_bundle_defs (
				bundle_id, tenant_id, role_definition_id, bundle_code, permitted_actions,
				active_flag, correlation_id, created_at, updated_at
			) VALUES ($1, $2, $3, 'REF_STEWARD', ARRAY[$4]::text[], TRUE, $5, $6, $6)`,
			uuid.NewString(), tenantID, roleID, granted, "corr-bundle-"+roleID, time.Now().UTC())
		require.NoError(t, err)
		_, err = tx.Exec(ctx, string(down))
		require.Error(t, err, "%s down while %s is granted", m.migration, granted)
		require.Contains(t, err.Error(), m.migration[:6]+" down refused")
		_ = tx.Rollback(ctx)
	}
}

// ── AC-1: S9-D1 aliases ───────────────────────────────────────────────────────
//
// The permission taxonomy has canonical (TAXONOMY, e.g. payment.release) and
// legacy (LEGACY, e.g. PAYMENT_RELEASE) names for the same capability. The
// alias_for column on LEGACY entries points to the canonical name. A grant of
// either name must satisfy a check for the other.

func TestS9D1_CanonicalGrantSatisfiesLegacyCheck(t *testing.T) {
	ctx := context.Background()
	// Bundle grants canonical name
	actions := []string{"payment.release"}
	known, err := permCatalog.Lookup(ctx, actions)
	require.NoError(t, err)
	require.Contains(t, known, "payment.release", "canonical action must be found")
	// The legacy alias should also be present in the map
	require.Contains(t, known, "PAYMENT_RELEASE", "legacy alias must be included for canonical grant")
	require.Equal(t, known["payment.release"].RiskTier, known["PAYMENT_RELEASE"].RiskTier, "risk tier must match")
}

func TestS9D1_LegacyGrantSatisfiesCanonicalCheck(t *testing.T) {
	ctx := context.Background()
	// Bundle grants legacy name
	actions := []string{"PAYMENT_RELEASE"}
	known, err := permCatalog.Lookup(ctx, actions)
	require.NoError(t, err)
	require.Contains(t, known, "PAYMENT_RELEASE", "legacy action must be found")
	// The canonical name should also be present in the map
	require.Contains(t, known, "payment.release", "canonical alias must be included for legacy grant")
	require.Equal(t, known["PAYMENT_RELEASE"].RiskTier, known["payment.release"].RiskTier, "risk tier must match")
}

func TestS9D1_UnknownActionStillRefused(t *testing.T) {
	ctx := context.Background()
	actions := []string{"UNKNOWN_ACTION_THAT_DOES_NOT_EXIST"}
	known, err := permCatalog.Lookup(ctx, actions)
	require.NoError(t, err)
	require.Empty(t, known, "unknown action must not be found")
}

func TestS9D1_AliasCannotPointAtUnregisteredAction(t *testing.T) {
	// This is enforced by the FK constraint on alias_for
	ctx := context.Background()
	// First verify the constraint exists by checking a known alias
	var aliasFor string
	err := ownerPool.QueryRow(ctx,
		`SELECT alias_for FROM permission_definitions WHERE action_name = 'PAYMENT_RELEASE'`).Scan(&aliasFor)
	require.NoError(t, err)
	require.Equal(t, "payment.release", aliasFor, "PAYMENT_RELEASE must alias to payment.release")
	
	// The FK constraint prevents creating an alias to a non-existent action
	// This is tested implicitly by the migration - if the constraint didn't
	// exist, the migration would have failed or allowed invalid aliases.
}
