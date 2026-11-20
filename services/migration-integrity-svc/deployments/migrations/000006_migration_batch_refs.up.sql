-- Migration: 000006_migration_batch_refs.up.sql
--
-- migration_batch_refs is the "program/tracking" layer that ties one
-- migration-integrity-svc validation run (migration_jobs.id) to the real
-- posting batch that general-ledger-svc owns and executes
-- (general_ledger.migration_batches.batch_id). It is deliberately NOT a copy
-- of general-ledger-svc's batch: it holds no lines, no account mappings, no
-- posting state machine of its own -- those stay exactly where they are,
-- in general-ledger-svc. This table only records which integrity job was
-- promoted to which GL batch, under which GL book, and the last lifecycle
-- stage this service observed GL report back (STAGE -> APPROVE -> POST ->
-- RECONCILE), so a MigrationCertificate can point at real evidence instead
-- of asserting its own.
CREATE TABLE IF NOT EXISTS migration_batch_refs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    job_id UUID NOT NULL REFERENCES migration_jobs(id),
    batch_code VARCHAR(128) NOT NULL,

    -- general-ledger-svc's own identifiers for the batch this promotion
    -- produced. NULL until the Stage call to general-ledger-svc succeeds.
    gl_book_id VARCHAR(64) NOT NULL,
    gl_batch_id VARCHAR(64),
    gl_journal_id VARCHAR(64),
    gl_journal_number VARCHAR(128),

    -- PENDING -> STAGED -> APPROVED -> POSTED -> RECONCILED, or FAILED at
    -- any step. This is the status of the PROMOTION, not a re-statement of
    -- general-ledger-svc's batch status -- GL's /v1/migration-batches/{id}
    -- remains the one authoritative read for that.
    status VARCHAR(16) NOT NULL DEFAULT 'PENDING',
    reconciliation_status VARCHAR(16), -- MATCHED or BREAK, copied from GL's Reconcile result once observed
    failure_reason TEXT,

    created_by VARCHAR(256) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT migration_batch_refs_status_valid
        CHECK (status IN ('PENDING', 'STAGED', 'APPROVED', 'POSTED', 'RECONCILED', 'FAILED')),
    CONSTRAINT migration_batch_refs_recon_valid
        CHECK (reconciliation_status IS NULL OR reconciliation_status IN ('MATCHED', 'BREAK')),
    -- One promotion per (tenant, job): a job is promoted at most once. A
    -- second promotion attempt is handled as an idempotent read of the
    -- first, never a second GL batch for the same validated extract.
    CONSTRAINT migration_batch_refs_job_unique UNIQUE (tenant_id, job_id)
);

ALTER TABLE migration_batch_refs ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS migration_batch_refs_isolation ON migration_batch_refs;
CREATE POLICY migration_batch_refs_isolation ON migration_batch_refs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX IF NOT EXISTS idx_migration_batch_refs_entity ON migration_batch_refs(tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_migration_batch_refs_gl_batch ON migration_batch_refs(tenant_id, gl_batch_id);
