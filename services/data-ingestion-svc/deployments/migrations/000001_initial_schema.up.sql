-- Schema for data-ingestion-svc (DATA-01)
-- Migration: 000001_initial_schema.up.sql

-- Ingestion Runs: one ingestion attempt
CREATE TABLE IF NOT EXISTS ingestion_runs (
    run_id              TEXT PRIMARY KEY
        CHECK (run_id ~ '^dir_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id           VARCHAR(64) NOT NULL,
    source_id           VARCHAR(128) NOT NULL,
    status              VARCHAR(32) NOT NULL DEFAULT 'Draft', -- Draft, Running, PartiallyQuarantined, Completed, Failed, Superseded
    started_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    closed_at           TIMESTAMPTZ,
    checkpoint_ref      VARCHAR(128),
    created_by          VARCHAR(128) NOT NULL,
    residency_region    VARCHAR(64) NOT NULL,
    classification      VARCHAR(64) NOT NULL,
    purpose             VARCHAR(128) NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Source Checkpoints: durable watermark per source
CREATE TABLE IF NOT EXISTS source_checkpoints (
    checkpoint_id       TEXT PRIMARY KEY
        CHECK (checkpoint_id ~ '^dsc_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    source_id           VARCHAR(128) NOT NULL,
    tenant_id           VARCHAR(64) NOT NULL,
    last_position       TEXT NOT NULL,
    last_committed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    schema_version      VARCHAR(32) NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (source_id, tenant_id)
);

-- Landing Objects: one committed batch's landed records
CREATE TABLE IF NOT EXISTS landing_objects (
    landing_id          TEXT PRIMARY KEY
        CHECK (landing_id ~ '^dlo_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    run_id              TEXT NOT NULL REFERENCES ingestion_runs(run_id) ON DELETE CASCADE,
    source_id           VARCHAR(128) NOT NULL,
    tenant_id           VARCHAR(64) NOT NULL,
    record_count        INT NOT NULL DEFAULT 0,
    content_hash        VARCHAR(64) NOT NULL,
    schema_version      VARCHAR(32) NOT NULL,
    classification      VARCHAR(64) NOT NULL,
    residency_region    VARCHAR(64) NOT NULL,
    landed_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    landed_by           VARCHAR(128) NOT NULL
);

-- Quarantine Items: batches/records that failed validation
CREATE TABLE IF NOT EXISTS quarantine_items (
    quarantine_id       TEXT PRIMARY KEY
        CHECK (quarantine_id ~ '^dqi_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    run_id              TEXT NOT NULL REFERENCES ingestion_runs(run_id) ON DELETE CASCADE,
    source_id           VARCHAR(128) NOT NULL,
    tenant_id           VARCHAR(64) NOT NULL,
    reason              TEXT NOT NULL,
    payload_ref         TEXT NOT NULL,
    schema_version      VARCHAR(32) NOT NULL,
    classification      VARCHAR(64) NOT NULL,
    residency_region    VARCHAR(64) NOT NULL,
    quarantined_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    quarantined_by      VARCHAR(128) NOT NULL
);

-- Landed Records: one row per source event actually landed, keyed by
-- (tenant, source, dedup_key). This is the real enforcement point for
-- "duplicate source event does not duplicate analytical facts" — a
-- resubmitted event (same dedup key) hits the UNIQUE constraint and is
-- excluded from the batch's landed count, never counted or stored twice,
-- regardless of which CommitBatch call or run it arrives under.
-- landing_id's FK is DEFERRABLE INITIALLY DEFERRED: CommitBatch needs to
-- know how many records actually landed (post-dedup) BEFORE it can write
-- the landing_objects row itself, so landed_records rows are inserted
-- first, referencing a landing_id whose parent row is written moments
-- later in the same transaction — valid as long as it exists by COMMIT.
CREATE TABLE IF NOT EXISTS landed_records (
    tenant_id           VARCHAR(64) NOT NULL,
    source_id           VARCHAR(128) NOT NULL,
    dedup_key           VARCHAR(256) NOT NULL,
    landing_id          TEXT NOT NULL REFERENCES landing_objects(landing_id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    source_event_id     VARCHAR(256) NOT NULL,
    landed_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, source_id, dedup_key)
);

ALTER TABLE landed_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE landed_records FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS landed_records_tenant_isolation ON landed_records;
CREATE POLICY landed_records_tenant_isolation ON landed_records
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX IF NOT EXISTS idx_landed_records_landing ON landed_records(landing_id);

-- Idempotency Keys: for replay-safe writes. Scoped per tenant so two
-- different organizations' principals can never collide on the same key.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           VARCHAR(64) NOT NULL,
    owner_scope         VARCHAR(32) NOT NULL,
    principal_id        VARCHAR(128) NOT NULL,
    idempotency_key     VARCHAR(128) NOT NULL,
    operation           VARCHAR(64) NOT NULL,
    request_sha256      VARCHAR(64) NOT NULL,
    resource_id         VARCHAR(128) NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, owner_scope, principal_id, idempotency_key)
);

-- Outbox Events: transactional outbox for event emission
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type      VARCHAR(64) NOT NULL,
    aggregate_id        VARCHAR(128) NOT NULL,
    event_type          VARCHAR(64) NOT NULL,
    payload             JSONB NOT NULL,
    tenant_id           VARCHAR(64),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at        TIMESTAMPTZ,
    publish_attempts    INT NOT NULL DEFAULT 0,
    last_error          TEXT
);

-- Enable RLS on all tables
ALTER TABLE ingestion_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE source_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE landing_objects ENABLE ROW LEVEL SECURITY;
ALTER TABLE quarantine_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;

-- Drop policies if exist to ensure clean setup
DROP POLICY IF EXISTS ingestion_runs_tenant_isolation ON ingestion_runs;
DROP POLICY IF EXISTS source_checkpoints_tenant_isolation ON source_checkpoints;
DROP POLICY IF EXISTS landing_objects_tenant_isolation ON landing_objects;
DROP POLICY IF EXISTS quarantine_items_tenant_isolation ON quarantine_items;
DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;

-- Create Tenant RLS policies (FORCE ROW LEVEL SECURITY)
ALTER TABLE ingestion_runs FORCE ROW LEVEL SECURITY;
ALTER TABLE source_checkpoints FORCE ROW LEVEL SECURITY;
ALTER TABLE landing_objects FORCE ROW LEVEL SECURITY;
ALTER TABLE quarantine_items FORCE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;

CREATE POLICY ingestion_runs_tenant_isolation ON ingestion_runs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY source_checkpoints_tenant_isolation ON source_checkpoints
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY landing_objects_tenant_isolation ON landing_objects
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY quarantine_items_tenant_isolation ON quarantine_items
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY outbox_events_tenant_isolation ON outbox_events
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_ingestion_runs_tenant_source ON ingestion_runs(tenant_id, source_id);
CREATE INDEX IF NOT EXISTS idx_ingestion_runs_tenant_status ON ingestion_runs(tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_source_checkpoints_tenant_source ON source_checkpoints(tenant_id, source_id);
CREATE INDEX IF NOT EXISTS idx_landing_objects_run ON landing_objects(run_id);
CREATE INDEX IF NOT EXISTS idx_landing_objects_tenant_source ON landing_objects(tenant_id, source_id);
CREATE INDEX IF NOT EXISTS idx_quarantine_items_run ON quarantine_items(run_id);
CREATE INDEX IF NOT EXISTS idx_quarantine_items_tenant_source ON quarantine_items(tenant_id, source_id);
CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished ON outbox_events(published_at) WHERE published_at IS NULL;

-- Immutability triggers for closed runs (reject UPDATE/DELETE on immutable tables)
-- LandingObjects are immutable from the moment they are written: a
-- CommitBatch call is the fact that a batch landed, evidence for later
-- DATA-03 lineage/DATA-02 DQ certification — it is never legitimately
-- edited afterward, whether the owning run is still open or closed.
CREATE OR REPLACE FUNCTION enforce_landing_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'landing object is immutable';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_landing_immutability ON landing_objects;
CREATE TRIGGER trigger_landing_immutability
    BEFORE UPDATE OR DELETE ON landing_objects
    FOR EACH ROW EXECUTE FUNCTION enforce_landing_immutability();

-- landed_records is the dedup ledger itself — never legitimately edited
-- (a "duplicate" is refused by the PRIMARY KEY, not by rewriting a row).
CREATE OR REPLACE FUNCTION enforce_landed_record_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'landed record is immutable';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_landed_record_immutability ON landed_records;
CREATE TRIGGER trigger_landed_record_immutability
    BEFORE UPDATE OR DELETE ON landed_records
    FOR EACH ROW EXECUTE FUNCTION enforce_landed_record_immutability();

-- QuarantineItems are always immutable once written
CREATE OR REPLACE FUNCTION enforce_quarantine_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'quarantine item is immutable';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_quarantine_immutability ON quarantine_items;
CREATE TRIGGER trigger_quarantine_immutability
    BEFORE UPDATE OR DELETE ON quarantine_items
    FOR EACH ROW EXECUTE FUNCTION enforce_quarantine_immutability();

-- SourceCheckpoints are deliberately NOT immutable: advancing the
-- watermark on every successful CommitBatch is the checkpoint's whole
-- purpose (DATA-01's own recovery requirement — "restart resumes exactly
-- where it left off"). Only DELETE is blocked, so a checkpoint can never
-- be silently discarded, only advanced or explicitly replayed via
-- ReplayFromCheckpoint.
CREATE OR REPLACE FUNCTION enforce_checkpoint_no_delete()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'source checkpoint cannot be deleted, only advanced or replayed';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_checkpoint_no_delete ON source_checkpoints;
CREATE TRIGGER trigger_checkpoint_no_delete
    BEFORE DELETE ON source_checkpoints
    FOR EACH ROW EXECUTE FUNCTION enforce_checkpoint_no_delete();

-- IngestionRuns: only allow status transitions per lifecycle, and a
-- terminal run (Completed/Failed/Superseded) is immutable outright — not
-- just its status column. Checking OLD.status <> NEW.status alone would
-- let a same-status UPDATE (e.g. touching checkpoint_ref) slip through
-- unchecked on a terminal row, since the whole CASE below is skipped when
-- status doesn't change.
CREATE OR REPLACE FUNCTION enforce_run_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF OLD.status IN ('Completed', 'Failed', 'Superseded') THEN
            RAISE EXCEPTION 'ingestion run is terminal (%) and immutable', OLD.status;
        END IF;
        IF OLD.status <> NEW.status THEN
            CASE OLD.status
                WHEN 'Draft' THEN
                    IF NEW.status NOT IN ('Running', 'Failed', 'Superseded') THEN
                        RAISE EXCEPTION 'invalid status transition from Draft to %', NEW.status;
                    END IF;
                WHEN 'Running' THEN
                    IF NEW.status NOT IN ('PartiallyQuarantined', 'Completed', 'Failed', 'Superseded') THEN
                        RAISE EXCEPTION 'invalid status transition from Running to %', NEW.status;
                    END IF;
                WHEN 'PartiallyQuarantined' THEN
                    IF NEW.status NOT IN ('Completed', 'Failed', 'Superseded') THEN
                        RAISE EXCEPTION 'invalid status transition from PartiallyQuarantined to %', NEW.status;
                    END IF;
                ELSE
                    RAISE EXCEPTION 'unknown status %', OLD.status;
            END CASE;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_run_lifecycle ON ingestion_runs;
CREATE TRIGGER trigger_run_lifecycle
    BEFORE UPDATE ON ingestion_runs
    FOR EACH ROW EXECUTE FUNCTION enforce_run_lifecycle();

-- Updated_at trigger for ingestion_runs
CREATE OR REPLACE FUNCTION update_ingestion_run_updated_at()
RETURNS trigger AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_ingestion_run_updated_at ON ingestion_runs;
CREATE TRIGGER trigger_ingestion_run_updated_at
    BEFORE UPDATE ON ingestion_runs
    FOR EACH ROW EXECUTE FUNCTION update_ingestion_run_updated_at();