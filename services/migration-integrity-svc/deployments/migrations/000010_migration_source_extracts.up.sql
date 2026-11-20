-- Migration: 000010_migration_source_extracts.up.sql
--
-- SourceExtract (ZS-MIG-001 §4): a frozen, versioned record of one landing
-- of source data -- extract_id, source watermark, generated_at, the
-- files/objects that landed, a manifest hash, record count and amount
-- controls. MIG-INV-03: "Source extracts are immutable after landing;
-- corrections create new extract versions." This table is therefore
-- append-only: there is no UPDATE path anywhere in this service for it (no
-- handler, no store method), and the triggers below make that the actual
-- database guarantee rather than a convention the application layer could
-- forget -- the exact pattern services/accounting-period-svc uses for its
-- own append-only period_state_history (deployments/migrations/
-- 000002_immutability.up.sql): forbid UPDATE, DELETE and TRUNCATE outright.
--
-- A "correction" is a new row: same extract_id, version = previous max + 1
-- (see store.CreateExtract, which pg_advisory_xact_lock's the (tenant,
-- extract_id) pair before computing the next version so two concurrent
-- corrections of the same extract can never race to the same version
-- number).
CREATE TABLE IF NOT EXISTS migration_source_extracts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    program_id UUID REFERENCES migration_programs(id), -- nullable: an extract can be registered ahead of a program existing

    -- extract_id is the caller-facing business key; version starts at 1 and
    -- increments per correction. (tenant_id, extract_id, version) is the
    -- natural key of one immutable landing.
    extract_id VARCHAR(128) NOT NULL,
    version INT NOT NULL DEFAULT 1,

    source_watermark VARCHAR(256) NOT NULL, -- e.g. a source-system cursor/LSN/timestamp marking what this extract covers
    generated_at TIMESTAMPTZ NOT NULL,      -- when the SOURCE system produced this extract (not when this service recorded it)
    manifest_hash VARCHAR(128) NOT NULL,    -- content hash of the extract manifest/files, so a later re-submission under a different name is still detectable
    record_count INT NOT NULL,
    files JSONB NOT NULL DEFAULT '[]'::jsonb,           -- the files/objects that landed (paths/keys), MIG-INV-03's "files/objects"
    amount_controls JSONB NOT NULL DEFAULT '{}'::jsonb, -- control-total map, same shape as migration_batch_refs' control totals: {name: {total, count, reference}}

    created_by VARCHAR(256) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT migration_source_extracts_version_positive CHECK (version > 0),
    CONSTRAINT migration_source_extracts_record_count_nonneg CHECK (record_count >= 0),
    CONSTRAINT migration_source_extracts_unique_version UNIQUE (tenant_id, extract_id, version)
);

ALTER TABLE migration_source_extracts ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS migration_source_extracts_isolation ON migration_source_extracts;
CREATE POLICY migration_source_extracts_isolation ON migration_source_extracts
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX IF NOT EXISTS idx_migration_source_extracts_entity ON migration_source_extracts(tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_migration_source_extracts_extract_id ON migration_source_extracts(tenant_id, extract_id);
CREATE INDEX IF NOT EXISTS idx_migration_source_extracts_program ON migration_source_extracts(tenant_id, program_id);

-- ─── Immutability: no UPDATE, DELETE or TRUNCATE, ever ──────────────────────
CREATE OR REPLACE FUNCTION migration_source_extract_forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% on % is forbidden: source extracts are immutable after landing (MIG-INV-03); register a new version instead', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_migration_source_extracts_append_only
    BEFORE UPDATE OR DELETE ON migration_source_extracts
    FOR EACH ROW EXECUTE FUNCTION migration_source_extract_forbid_mutation();
-- TRUNCATE bypasses row triggers; forbid it too.
CREATE TRIGGER trg_migration_source_extracts_no_truncate
    BEFORE TRUNCATE ON migration_source_extracts
    FOR EACH STATEMENT EXECUTE FUNCTION migration_source_extract_forbid_mutation();
