-- Migration: 000005_add_access_decision_tenant.up.sql
--
-- Gives access_decision_log the tenant_id column it never had, and the row
-- level security policy that column makes possible.
--
-- WHY. GET /v1/access-decisions/{id} required no authentication and its
-- query carried no tenant or entity predicate, so anyone who could reach
-- the port could walk decision ids and read principal_id, legal_entity_id,
-- action_type, decision_outcome and decision_basis for every tenant on the
-- platform. decision_basis carries `sod:conflict_with=<action>`, so that is
-- not only a map of who may do what — it names where the segregation-of-
-- duties tripwires are. 000004_add_rls deliberately left this table alone
-- ("carries no tenant_id at all ... fabricating a tenant_id column on
-- tables that were never given one is a data-model change, not an RLS
-- migration"), and tracked the column as Priority 6 item 82. This is that
-- change, made because the read it protects had no other control.
--
-- NULLABLE, deliberately. /v1/authorize is called by ~60 services and most
-- do not send a tenant at all yet (see the same pass's fix to make the
-- endpoint prefer the verified X-Tenant-Id header). Requiring a tenant here
-- would turn a missing header into a failed authorization for every one of
-- those callers — a platform-wide outage in place of an incomplete record.
-- Rows already in the table predate the column and are NULL for the same
-- reason. A NULL-tenant row is honest about being unattributed, and is NOT
-- readable through the tenant-scoped GET route: PgStore's
-- FindAccessDecisionByID carries `tenant_id = $2` explicitly, and NULL is
-- never equal to anything.

ALTER TABLE access_decision_log ADD COLUMN tenant_id UUID;

COMMENT ON COLUMN access_decision_log.tenant_id IS
    'Verified tenant scope the decision was evaluated in. NULL when the caller supplied none (pre-000005 rows, and callers that send no tenant); NULL rows are not readable via GET /v1/access-decisions/{id}.';

-- Rationale retrieval is always by (id, tenant) now, and audit sweeps are
-- per-tenant-and-time.
CREATE INDEX idx_access_decision_log_tenant ON access_decision_log (tenant_id, decided_at DESC);

-- The policy admits tenant_id IS NULL on BOTH sides, exactly as sod_rules'
-- policy does, and for a concrete reason rather than symmetry: the insert in
-- RecordAccessDecision uses RETURNING, and Postgres applies the SELECT side
-- of a FOR ALL policy to an INSERT ... RETURNING. A USING clause that
-- excluded NULL would therefore make recording a tenantless decision fail
-- outright — fail-closed on the wrong thing, since the alternative to
-- recording that decision is not recording it.
--
-- So the tenant isolation of the READ path is the store's explicit
-- `tenant_id = $2` predicate, and this policy is the backstop that keeps one
-- tenant's rows out of another tenant's unfiltered query. Both are asserted
-- separately in internal/store's isolation tests, through a NOSUPERUSER
-- NOBYPASSRLS role — a superuser bypasses row security unconditionally, so
-- a suite that only uses the migration-running connection proves nothing
-- about what is written here.
ALTER TABLE access_decision_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_decision_log FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON access_decision_log
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
