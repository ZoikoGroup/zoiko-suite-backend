//go:build integration

// Tenant isolation at the database (R4, migration 000032): the three tables that
// hold a tenant's own facts answer only the tenant the transaction runs as.
//
// The suite's pool connects as a superuser, which row-level security never
// binds. These tests read through a pool whose sessions SET ROLE to a plain
// role, as the service's runtime role is, so the policies are what is tested.
package registryit_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

const otherTenant = "99999999-8888-4777-8666-555555555555"

// probe returns a store and a pool whose sessions run as jur_rls_probe, a role
// with table privileges and no superuser or BYPASSRLS attribute.
func probe(t *testing.T) (*store.PgStore, *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'jur_rls_probe') THEN
				CREATE ROLE jur_rls_probe NOLOGIN NOSUPERUSER NOBYPASSRLS;
			END IF;
		END $$;
		GRANT USAGE ON SCHEMA public TO jur_rls_probe;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO jur_rls_probe;
		GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO jur_rls_probe;`)
	require.NoError(t, err)
	cfg, err := pgxpool.ParseConfig(pool.Config().ConnString())
	require.NoError(t, err)
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET ROLE jur_rls_probe`)
		return err
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(p.Close)
	return store.New(p, zap.NewNop()), p
}

// countAs counts rows matching sql in a transaction running as tenant (and,
// with cross, with the operators' cross-tenant read of decision evidence).
func countAs(t *testing.T, p *pgxpool.Pool, tenant string, cross bool, sql string, args ...any) int {
	t.Helper()
	tx, err := p.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
	require.NoError(t, err)
	if cross {
		_, err = tx.Exec(ctx, `SELECT set_config('app.cross_tenant', 'on', true)`)
		require.NoError(t, err)
	}
	var n int
	require.NoError(t, tx.QueryRow(ctx, sql, args...).Scan(&n))
	return n
}

func TestR4_EntityApplicabilityIsTenantIsolated(t *testing.T) {
	ps, p := probe(t)
	jur := seedJurisdiction(t, "RLSE")
	entityA, entityB := uuid.NewString(), uuid.NewString()
	apply := func(tenant, entity string) {
		applied, err := ps.ApplyEntityApplicability(ctx, "evt-"+uuid.NewString(), "entity.created", domain.EntityJurisdictionApplicability{
			ApplicabilityID: "primary:" + entity, TenantID: tenant, LegalEntityID: entity, JurisdictionID: jur.JurisdictionID,
			ApplicabilityType: "ENTITY_PRIMARY", EffectiveFrom: time.Now().UTC().Add(-time.Hour), SourceObjectVersion: 1})
		require.NoError(t, err)
		require.True(t, applied)
	}
	apply(itTenant, entityA)
	apply(otherTenant, entityB)

	got, err := ps.ListEntityJurisdictions(ctx, itTenant, entityA, time.Time{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	got, err = ps.ListEntityJurisdictions(ctx, otherTenant, entityA, time.Time{})
	require.NoError(t, err)
	require.Empty(t, got, "another tenant's entity is not readable")

	const q = `SELECT count(*) FROM entity_jurisdiction_applicability WHERE legal_entity_id = ANY($1)`
	both := []string{entityA, entityB}
	require.Equal(t, 1, countAs(t, p, itTenant, false, q, both), "the database answers only the transaction's tenant, without a WHERE on tenant_id")
	require.Equal(t, 1, countAs(t, p, otherTenant, false, q, both))
	require.Equal(t, 0, countAs(t, p, "", false, q, both), "no tenant, no rows")

	// Writing a row for another tenant is refused by the policy's WITH CHECK.
	tx, err := p.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, otherTenant)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO entity_jurisdiction_applicability (applicability_id, tenant_id, legal_entity_id, jurisdiction_id,
		applicability_type, effective_from, source_object_version, source_event_type, source_event_id)
		VALUES ($1, $2, $3, $4::uuid, 'ENTITY_PRIMARY', NOW(), 1, 'entity.created', 'x')`,
		"primary:"+uuid.NewString(), itTenant, uuid.NewString(), jur.JurisdictionID)
	require.ErrorContains(t, err, "row-level security")
}

func TestR4_SubmissionsAreTenantIsolated(t *testing.T) {
	ps, p := probe(t)
	s := newSubmissionScenario(t)
	s.release(t)
	id := subID(s.register(t, map[string]any{"subject_ref": "INV-RLS-" + uuid.NewString()[:6]}, 201))

	var recorded string
	require.NoError(t, pool.QueryRow(ctx, `SELECT tenant_id FROM regulatory_submissions WHERE submission_id=$1`, id).Scan(&recorded))
	require.Equal(t, itTenant, recorded, "the submission records the tenant it was registered for")

	_, _, err := ps.GetSubmission(store.WithTenant(ctx, otherTenant), id)
	require.ErrorIs(t, err, domain.ErrSubmissionNotFound, "another tenant's submission is not found")
	_, _, err = ps.GetSubmission(store.WithTenant(ctx, ""), id)
	require.ErrorIs(t, err, domain.ErrSubmissionNotFound)
	got, _, err := ps.GetSubmission(store.WithTenant(ctx, itTenant), id)
	require.NoError(t, err)
	require.Equal(t, id, got.SubmissionID)

	require.Equal(t, 0, countAs(t, p, otherTenant, true, `SELECT count(*) FROM regulatory_submissions WHERE submission_id=$1`, id),
		"the operators' cross-tenant switch covers decision evidence only")
}

func TestR4_DecisionEvidenceIsTenantIsolated(t *testing.T) {
	ps, p := probe(t)
	c := releasedPack(t, 0)
	code, r := c.e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	mustStatus(t, 200, code, r)
	id := r["decision_id"].(string)

	var recorded *string
	require.NoError(t, pool.QueryRow(ctx, `SELECT tenant_id FROM rule_decision_evidence WHERE decision_id=$1`, id).Scan(&recorded))
	require.NotNil(t, recorded)
	require.Equal(t, itTenant, *recorded)

	_, err := ps.GetDecision(store.WithTenant(ctx, otherTenant), id)
	require.ErrorIs(t, err, domain.ErrDecisionNotFound, "another tenant's decision is not found")
	got, err := ps.GetDecision(store.WithTenant(ctx, itTenant), id)
	require.NoError(t, err)
	require.Equal(t, id, got.DecisionID)

	const q = `SELECT count(*) FROM rule_decision_evidence WHERE decision_id=$1`
	require.Equal(t, 0, countAs(t, p, otherTenant, false, q, id))
	require.Equal(t, 1, countAs(t, p, otherTenant, true, q, id), "an operator's cross-tenant read (remediation, rollback, metrics) sees it")
}
