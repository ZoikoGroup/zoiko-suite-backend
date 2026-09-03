-- Migration: 000009_add_migration_accounting.up.sql
--
-- ACC-17 (Opening Balance & Migration): "owns Migration accounting batch/
-- crosswalk/certification. Must never own: Bypass of ACC-04/05" — opening
-- postings go through general-ledger-svc's real journal lifecycle and this
-- service's own real period status, exactly like every other capability;
-- there is no separate "bulk import" path that skips Create/Validate/Post
-- or period-lock checks. Fuller ownership: "MigrationAccountingBatch,
-- opening journal proposals, source crosswalk and reconciliation/
-- certification refs."
--
-- migration_batches is a normal mutable stateful row walking the spec's
-- own linear state model: "Planned → Loaded → Validated → Approved →
-- Posted → Reconciled → Certified; failed/quarantined as needed." Planned
-- collapses into LOADED in this v1 for the same reason ACC-10's Planned/
-- Calculated collapsed into Review: CreateMigrationAccountingBatch loads
-- every crosswalk entry synchronously in one call.
--
-- migration_crosswalk_entries is append-only calculation evidence — the
-- batch's own permanent record of every source-to-target line it was ever
-- given, unique per (batch_id, source_reference_id) so the same source
-- item can never be loaded into one batch twice — the closest real
-- enforcement this v1 has of the spec's own negative path, "Open AR
-- included both in history and opening state."

CREATE TABLE migration_batches (
    batch_id                   UUID PRIMARY KEY,
    tenant_id                    VARCHAR(255) NOT NULL,
    legal_entity_id               VARCHAR(255) NOT NULL,
    fiscal_period                  VARCHAR(20) NOT NULL,
    source_system_name              VARCHAR(255) NOT NULL,
    source_extract_hash              VARCHAR(255) NOT NULL, -- caller-declared hash of the legacy extract — permanent lineage evidence
    expected_row_count               INT NOT NULL,
    expected_total_debits             NUMERIC(18,2) NOT NULL,
    expected_total_credits            NUMERIC(18,2) NOT NULL,
    status                            VARCHAR(20) NOT NULL, -- LOADED|VALIDATED|APPROVED|POSTED|RECONCILED|CERTIFIED|QUARANTINED
    quarantine_reason                 TEXT,
    journal_id                        VARCHAR(255),
    created_at                        TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id           VARCHAR(255) NOT NULL,
    validated_at                      TIMESTAMP WITH TIME ZONE,
    approved_at                       TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id          VARCHAR(255),
    posted_at                         TIMESTAMP WITH TIME ZONE,
    reconciled_at                     TIMESTAMP WITH TIME ZONE,
    certified_at                      TIMESTAMP WITH TIME ZONE,
    certified_by_principal_id         VARCHAR(255),
    certification_reason              TEXT,
    UNIQUE (tenant_id, legal_entity_id, fiscal_period, source_system_name)
);

CREATE TABLE migration_crosswalk_entries (
    entry_id                UUID PRIMARY KEY,
    tenant_id                 VARCHAR(255) NOT NULL,
    batch_id                   UUID NOT NULL REFERENCES migration_batches(batch_id),
    source_reference_id         VARCHAR(255) NOT NULL, -- the legacy system's own row identifier
    source_account_code          VARCHAR(255) NOT NULL, -- as named in the legacy system, informational only
    target_account_code           VARCHAR(64) NOT NULL, -- resolved, chart-registered GL account
    debit_amount                   NUMERIC(18,2) NOT NULL DEFAULT 0,
    credit_amount                   NUMERIC(18,2) NOT NULL DEFAULT 0,
    UNIQUE (batch_id, source_reference_id)
);

CREATE OR REPLACE FUNCTION reject_crosswalk_entry_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'migration_crosswalk_entries is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_crosswalk_update
    BEFORE UPDATE ON migration_crosswalk_entries
    FOR EACH ROW EXECUTE FUNCTION reject_crosswalk_entry_mutation();
CREATE TRIGGER trg_reject_crosswalk_delete
    BEFORE DELETE ON migration_crosswalk_entries
    FOR EACH ROW EXECUTE FUNCTION reject_crosswalk_entry_mutation();

ALTER TABLE migration_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE migration_batches FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON migration_batches
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE migration_crosswalk_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE migration_crosswalk_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON migration_crosswalk_entries
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_migration_batches_entity_period ON migration_batches (tenant_id, legal_entity_id, fiscal_period);
CREATE INDEX idx_migration_crosswalk_entries_batch ON migration_crosswalk_entries (tenant_id, batch_id);
