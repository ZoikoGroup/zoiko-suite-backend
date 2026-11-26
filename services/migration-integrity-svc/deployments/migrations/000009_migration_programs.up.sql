-- Migration: 000009_migration_programs.up.sql
--
-- MigrationProgram is the top-level tracking object ZS-MIG-001 §4 describes
-- above the batch: "this is one migration project" (e.g. one customer's
-- full onboarding), which can span many validation jobs and many promoted
-- batches over time (tenant, program_id, source estate, target scope,
-- owner, status, cutover strategy). It holds no posting state of its own --
-- that stays in general-ledger-svc via migration_batch_refs, same as the
-- rest of this "program/tracking" layer (see control_plane.go's package
-- doc). A program aggregates the batches that name it; it does not contain
-- them structurally.
CREATE TABLE IF NOT EXISTS migration_programs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,

    -- program_id is the caller-facing business key (e.g. "ACME-ONB-2026"),
    -- distinct from the internal UUID the way migration_batch_refs.batch_code
    -- is distinct from its id -- a program is referenced by name across many
    -- requests over the life of a migration project.
    program_id VARCHAR(128) NOT NULL,

    source_estate VARCHAR(256) NOT NULL,   -- the legacy system/estate being migrated off
    target_scope VARCHAR(256) NOT NULL,    -- what in the platform this program loads into
    owner_principal_id VARCHAR(256) NOT NULL,
    cutover_strategy VARCHAR(32) NOT NULL, -- BIG_BANG, PHASED, PARALLEL_RUN

    -- OPEN -> IN_PROGRESS -> COMPLETE. Linear and forward-only, same shape as
    -- accounting_periods' own guarded lifecycle (services/accounting-period-svc
    -- /deployments/migrations/000002_immutability.up.sql) -- see the Go-side
    -- guard in the handler/store for the actual transition check; the CHECK
    -- here only bounds the value domain.
    status VARCHAR(16) NOT NULL DEFAULT 'OPEN',

    created_by VARCHAR(256) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT migration_programs_status_valid
        CHECK (status IN ('OPEN', 'IN_PROGRESS', 'COMPLETE')),
    CONSTRAINT migration_programs_cutover_strategy_valid
        CHECK (cutover_strategy IN ('BIG_BANG', 'PHASED', 'PARALLEL_RUN')),
    -- One business program_id per tenant: callers address a program by this
    -- name repeatedly over the life of the project.
    CONSTRAINT migration_programs_program_id_unique UNIQUE (tenant_id, program_id)
);

ALTER TABLE migration_programs ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS migration_programs_isolation ON migration_programs;
CREATE POLICY migration_programs_isolation ON migration_programs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX IF NOT EXISTS idx_migration_programs_tenant ON migration_programs(tenant_id);

-- A MigrationBatchRef now belongs to a MigrationProgram. Additive only:
-- nullable, so every batch ref promoted before this migration (and any
-- promote call that still omits a program, which remains supported for
-- backward compatibility) keeps exactly the semantics it had -- "no program"
-- is a valid, pre-existing state, not a data migration this pass needs to
-- backfill.
ALTER TABLE migration_batch_refs
    ADD COLUMN IF NOT EXISTS program_id UUID REFERENCES migration_programs(id);

CREATE INDEX IF NOT EXISTS idx_migration_batch_refs_program ON migration_batch_refs(tenant_id, program_id);
