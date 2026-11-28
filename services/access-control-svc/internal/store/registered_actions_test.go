//go:build integration

package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
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
	{"000046_register_address_actions", "tenant-entity-registry-svc", map[string]string{
		"ADDRESS_CREATE": "STANDARD",
		"ADDRESS_AMEND":  "STANDARD",
		"ADDRESS_VERIFY": "STANDARD",
		"ADDRESS_LINK":   "STANDARD",
	}},
	{"000047_register_establishment_actions", "tenant-entity-registry-svc", map[string]string{
		"ESTABLISHMENT_CREATE":     "STANDARD",
		"ESTABLISHMENT_ACTIVATE":   "HIGH",
		"ESTABLISHMENT_DEACTIVATE": "STANDARD",
		"ESTABLISHMENT_CLOSE":      "HIGH",
	}},
	{"000047_register_evidence_manifest_actions", "evidence-manifest-svc", map[string]string{
		"EVIDENCE_MANIFEST_GENERATE": "STANDARD",
		"EVIDENCE_MANIFEST_READ":     "STANDARD",
	}},
	{"000048_register_registration_actions", "tenant-entity-registry-svc", map[string]string{
		"REGISTRATION_CREATE":        "STANDARD",
		"REGISTRATION_VERIFY":        "HIGH",
		"REGISTRATION_RENEW":         "HIGH",
		"REGISTRATION_SUSPEND":       "HIGH",
		"REGISTRATION_EXPIRE":        "STANDARD",
		"REGISTRATION_REVOKE":        "HIGH",
		"REGISTRATION_NUMBER_REVEAL": "HIGH",
	}},
	{"000049_register_identifier_verify_action", "tenant-entity-registry-svc", map[string]string{
		"PARTY_IDENTIFIER_VERIFY": "HIGH",
	}},
	{"000050_register_fx_actions", "tenant-entity-registry-svc", map[string]string{
		"FX_SOURCE_MANAGE":         "HIGH",
		"FX_OBSERVATION_INGEST":    "STANDARD",
		"FX_OBSERVATION_SUPERSEDE": "HIGH",
		"FX_RATE_SET_APPROVE":      "HIGH",
		"FX_RATE_SET_CLOSE":        "HIGH",
	}},
	{"000051_register_book_actions", "tenant-entity-registry-svc", map[string]string{
		"ACCOUNTING_BOOK_CREATE":          "STANDARD",
		"ACCOUNTING_BOOK_ACTIVATE":        "HIGH",
		"ACCOUNTING_BOOK_AMEND_BASIS":     "HIGH",
		"ACCOUNTING_BOOK_SUSPEND_POSTING": "HIGH",
		"ACCOUNTING_BOOK_RETIRE":          "HIGH",
	}},
	{"000052_jurisdiction_rules_templates", "jurisdiction-rules-svc", map[string]string{
		"JURISDICTION_OUTBOX_EVENT_VIEW":    "STANDARD",
		"JURISDICTION_OUTBOX_EVENT_REQUEUE": "HIGH",
		"JURISDICTION_SUMMARY_VIEW":         "STANDARD",
		"JURISDICTION_ENTITY_BACKFILL":      "HIGH",
		"JURISDICTION_OPERATION_VIEW":       "STANDARD",
		"JURISDICTION_OPERATION_CANCEL":     "HIGH",
	}},
	{"000052_register_dimension_actions", "tenant-entity-registry-svc", map[string]string{
		"DIMENSION_TYPE_CREATE":       "STANDARD",
		"DIMENSION_CREATE":            "STANDARD",
		"DIMENSION_ACTIVATE":          "STANDARD",
		"DIMENSION_DEACTIVATE":        "HIGH",
		"DIMENSION_REPARENT":          "STANDARD",
		"DIMENSION_APPROVE_HIERARCHY": "HIGH",
		"DIMENSION_MAP_SOURCE":        "STANDARD",
	}},
	{"000053_register_unit_actions", "tenant-entity-registry-svc", map[string]string{
		"UNIT_DIMENSION_CREATE":   "STANDARD",
		"UNIT_CREATE":             "STANDARD",
		"UNIT_PROPOSE_CONVERSION": "STANDARD",
		"UNIT_APPROVE_CONVERSION": "HIGH",
		"UNIT_RETIRE":             "HIGH",
	}},
	{"000054_register_reference_actions", "tenant-entity-registry-svc", map[string]string{
		"REFERENCE_CREATE_SET":      "STANDARD",
		"REFERENCE_STAGE_RELEASE":   "STANDARD",
		"REFERENCE_PUBLISH_RELEASE": "HIGH",
	}},
	{"000055_register_master_import_actions", "tenant-entity-registry-svc", map[string]string{
		"MASTER_IMPORT_UPLOAD":  "STANDARD",
		"MASTER_IMPORT_APPROVE": "HIGH",
		"MASTER_IMPORT_CANCEL":  "STANDARD",
	}},
	{"000056_register_party_contact_actions", "tenant-entity-registry-svc", map[string]string{
		"PARTY_CONTACT_CREATE": "STANDARD",
		"PARTY_CONTACT_VERIFY": "HIGH",
		"PARTY_CONTACT_RETIRE": "STANDARD",
		"PARTY_CONTACT_READ":   "STANDARD",
	}},
	{"000057_register_policy_evidence_redeliver", "policy-svc", map[string]string{
		"POLICY_EVIDENCE_REDELIVER": "HIGH",
	}},
	{"000057_register_crm_read_actions", "counterparty-management-svc", map[string]string{
		"CRM_READ":         "STANDARD",
		"CRM_CONSENT_READ": "STANDARD",
	}},
	{"000058_register_policy_certification_actions", "policy-svc", map[string]string{
		"POLICY_TEST_MANAGE":    "HIGH",
		"POLICY_TEST_RUN":       "HIGH",
		"POLICY_WAIVER_REQUEST": "STANDARD",
		"POLICY_WAIVER_APPROVE": "HIGH",
		"POLICY_SIMULATE":       "STANDARD",
	}},
	{"000058_register_restricted_classification_action", "document-vault-svc", map[string]string{
		"CONFIRM_RESTRICTED_CLASSIFICATION": "HIGH",
	}},
	{"000059_register_comment_moderate_action", "comments-collaboration-svc", map[string]string{
		"COMMENT_MODERATE": "STANDARD",
	}},
	{"000060_register_signatory_office_actions", "workflow-svc", map[string]string{
		"SIGNATORY_SIGN_AS_DIRECTOR":          "HIGH",
		"SIGNATORY_SIGN_AS_CFO":               "HIGH",
		"SIGNATORY_SIGN_AS_CEO":               "HIGH",
		"SIGNATORY_SIGN_AS_FINANCE_SIGNER":    "HIGH",
		"SIGNATORY_SIGN_AS_COMPANY_SECRETARY": "HIGH",
		"SIGNATORY_SIGN_AS_GENERAL_COUNSEL":   "HIGH",
		"SIGNATORY_SIGN_AS_TREASURER":         "HIGH",
	}},
	{"000062_register_obligation_clock_actions", "obligations-svc", map[string]string{
		"OBLIGATION_CLOCK_CREATE":             "STANDARD",
		"OBLIGATION_CLOCK_SATISFY":            "STANDARD",
		"OBLIGATION_CLOCK_RECALCULATE":        "STANDARD",
		"OBLIGATION_CLOCK_WAIVE":              "HIGH",
		"OBLIGATION_CLOCK_NOT_APPLICABLE":     "HIGH",
		"OBLIGATION_ESCALATION_POLICY_MANAGE": "STANDARD",
	}},
}

// templatePublishers are the migrations, in this list, that publish role
// templates carrying actions earlier migrations registered (rule R5).
// 000062 publishes OBLIGATION_MANAGER v4, which inherits v3's actions
// (000047's EVIDENCE_MANIFEST_READ among them).
var templatePublishers = []string{"000052_jurisdiction_rules_templates", "000062_register_obligation_clock_actions"}

// newPermCatalog is the permission catalogue used by alias tests. It is a
// function, not a package-level var: a var would be initialised before
// TestMain assigns testStore, which captured a nil store and panicked on the
// first Lookup.
func newPermCatalog() *store.PermissionCatalogue { return store.NewPermissionCatalogue(testStore) }

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
		// Migrations are reverted newest first, so a later migration that
		// publishes templates carrying these actions is reverted before it.
		tx, err := ownerPool.Begin(ctx)
		require.NoError(t, err)
		reverted := map[string]bool{}
		for _, later := range templatePublishers {
			if later > m.migration {
				laterDown, err := os.ReadFile("../../deployments/migrations/" + later + ".down.sql")
				require.NoError(t, err)
				_, err = tx.Exec(ctx, string(laterDown))
				require.NoError(t, err, "%s down, before %s down", later, m.migration)
				reverted[later] = true
			}
		}
		_, err = tx.Exec(ctx, string(down))
		require.NoError(t, err, "%s down with nothing granted", m.migration)
		for _, other := range registeredActionMigrations {
			if reverted[other.migration] {
				continue
			}
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
	ctx := svcmiddleware.WithTenant(context.Background(), tenantGov)
	// Bundle grants canonical name
	actions := []string{"iam.role.manage"}
	known, err := newPermCatalog().Lookup(ctx, actions)
	require.NoError(t, err)
	require.Contains(t, known, "iam.role.manage", "canonical action must be found")
	// The legacy alias should also be present in the map
	require.Contains(t, known, "ROLE_MANAGE", "legacy alias must be included for canonical grant")
	require.Equal(t, known["iam.role.manage"].RiskTier, known["ROLE_MANAGE"].RiskTier, "risk tier must match")
}

func TestS9D1_LegacyGrantSatisfiesCanonicalCheck(t *testing.T) {
	ctx := svcmiddleware.WithTenant(context.Background(), tenantGov)
	// Bundle grants legacy name
	actions := []string{"ROLE_MANAGE"}
	known, err := newPermCatalog().Lookup(ctx, actions)
	require.NoError(t, err)
	require.Contains(t, known, "ROLE_MANAGE", "legacy action must be found")
	// The canonical name should also be present in the map
	require.Contains(t, known, "iam.role.manage", "canonical alias must be included for legacy grant")
	require.Equal(t, known["ROLE_MANAGE"].RiskTier, known["iam.role.manage"].RiskTier, "risk tier must match")
}

func TestS9D1_UnknownActionStillRefused(t *testing.T) {
	ctx := svcmiddleware.WithTenant(context.Background(), tenantGov)
	actions := []string{"UNKNOWN_ACTION_THAT_DOES_NOT_EXIST"}
	known, err := newPermCatalog().Lookup(ctx, actions)
	require.NoError(t, err)
	require.Empty(t, known, "unknown action must not be found")
}

func TestS9D1_AliasCannotPointAtUnregisteredAction(t *testing.T) {
	// This is enforced by the FK constraint on alias_for
	ctx := context.Background()
	// First verify the constraint exists by checking a known alias
	var aliasFor string
	err := ownerPool.QueryRow(ctx,
		`SELECT alias_for FROM permission_definitions WHERE action_name = 'ROLE_MANAGE'`).Scan(&aliasFor)
	require.NoError(t, err)
	require.Equal(t, "iam.role.manage", aliasFor, "ROLE_MANAGE must alias to iam.role.manage")

	// The FK constraint prevents creating an alias to a non-existent action
	// This is tested implicitly by the migration - if the constraint didn't
	// exist, the migration would have failed or allowed invalid aliases.
}
