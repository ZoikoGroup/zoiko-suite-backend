//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"zoiko.io/access-control-svc/internal/domain"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
	"zoiko.io/access-control-svc/internal/store"
)

// Migration 000007 and the store halves of the 6 Oct 2026 guard fixes, driven
// through the NOBYPASSRLS app pool so the policies actually apply.

func TestProtectedCatalogueReadsTheTableAsTheAppRole(t *testing.T) {
	cat := store.NewProtectedCatalogue(store.New(appPool))
	got, err := cat.ListActive(svcmiddleware.WithTenant(context.Background(), tenantA))
	require.NoError(t, err)
	require.Contains(t, got, "PLATFORM_ADMIN", "000005's seed")
	require.Contains(t, got, "iam.role.manage", "000007's iam.* actions")
	require.Contains(t, got, "iam.assignment.grant")

	_, err = cat.ListActive(context.Background())
	require.ErrorIs(t, err, domain.ErrIdentityMissing, "no tenant, no read")
}

func TestProtectedCatalogueIsNotWritableByTheApp(t *testing.T) {
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, appPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO protected_permissions (action_name, description, category) VALUES ('X_TEST', 'x', 'x')`)
		return err
	})
	require.Error(t, err, "the app inserted into the platform catalogue")

	var n int64
	err = pgx.BeginFunc(ctx, appPool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE protected_permissions SET active_flag = false WHERE action_name = 'iam.role.manage'`)
		n = tag.RowsAffected()
		return err
	})
	require.NoError(t, err)
	require.Zero(t, n, "the app deactivated a protected action")
}

func TestRefusalTablesAreForceRLS(t *testing.T) {
	for _, table := range []string{"refused_escalations", "protected_permissions"} {
		var forced bool
		require.NoError(t, ownerPool.QueryRow(context.Background(),
			`SELECT relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&forced))
		require.True(t, forced, "%s is not FORCE ROW LEVEL SECURITY", table)
	}
}

func TestRefusalWithoutItsOwnTenantIsRecordedUnderTheRequestTenant(t *testing.T) {
	ctx := svcmiddleware.WithTenant(context.Background(), tenantA)
	corr := uuid.NewString()
	err := store.New(appPool).RecordRefusedEscalation(ctx, &domain.RefusedEscalation{
		// TenantID left empty, as the invalid_json / missing_fields refusals
		// send it: they are made before the handler resolves its tenant.
		LegalEntityID: "le-1", CorrelationID: corr, ActionType: "CREATE_ROLE",
		RefusalReason: "invalid_json", RequestedPayload: []byte(`{}`),
		ErrorCode: "invalid_json", ErrorMessage: "bad", CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err, "an early refusal was lost")

	var tenant string
	require.NoError(t, ownerPool.QueryRow(context.Background(),
		`SELECT tenant_id FROM refused_escalations WHERE correlation_id = $1`, corr).Scan(&tenant))
	require.Equal(t, tenantA, tenant)
}

func TestRefusalsAreAppendOnly(t *testing.T) {
	ctx := svcmiddleware.WithTenant(context.Background(), tenantA)
	corr := uuid.NewString()
	require.NoError(t, store.New(appPool).RecordRefusedEscalation(ctx, &domain.RefusedEscalation{
		TenantID: tenantA, LegalEntityID: "le-1", PrincipalID: "p", CorrelationID: corr,
		ActionType: "CREATE_BUNDLE", RefusalReason: "protected_action", RequestedPayload: []byte(`{}`),
		ErrorCode: "protected_action", ErrorMessage: "refused", CreatedAt: time.Now().UTC(),
	}))

	for _, stmt := range []string{
		`UPDATE refused_escalations SET refusal_reason = 'nothing' WHERE correlation_id = $1`,
		`DELETE FROM refused_escalations WHERE correlation_id = $1`,
	} {
		var n int64
		err := pgx.BeginFunc(context.Background(), appPool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", tenantA); err != nil {
				return err
			}
			tag, err := tx.Exec(context.Background(), stmt, corr)
			n = tag.RowsAffected()
			return err
		})
		require.NoError(t, err)
		require.Zero(t, n, "the tenant's own context altered its refusal record: %s", stmt)
	}

	var reason string
	require.NoError(t, ownerPool.QueryRow(context.Background(),
		`SELECT refusal_reason FROM refused_escalations WHERE correlation_id = $1`, corr).Scan(&reason))
	require.Equal(t, "protected_action", reason)

	// Reads stay tenant-scoped.
	var seen int
	require.NoError(t, pgx.BeginFunc(context.Background(), appPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", tenantB); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM refused_escalations WHERE correlation_id = $1`, corr).Scan(&seen)
	}))
	require.Zero(t, seen, "another tenant read the refusal")
}
