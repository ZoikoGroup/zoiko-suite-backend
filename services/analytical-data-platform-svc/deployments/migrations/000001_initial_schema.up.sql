-- Schema for analytical-data-platform-svc (DATA-04, ZS-SVC-N-001 §4)
-- Migration: 000001_initial_schema.up.sql

-- Analytical Datasets: the named, stable identity of a dataset.
CREATE TABLE IF NOT EXISTS analytical_datasets (
    dataset_id           TEXT         PRIMARY KEY
        CHECK (dataset_id ~ '^dad_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id              VARCHAR(64)  NOT NULL,
    name                     VARCHAR(128) NOT NULL,
    created_at                 TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                   VARCHAR(128) NOT NULL,
    UNIQUE (tenant_id, name)
);

-- Dataset Versions: one versioned build. Published versions are immutable
-- — a rebuild of published content always creates a NEW version, never an
-- edit of the one already live.
CREATE TABLE IF NOT EXISTS dataset_versions (
    version_id                TEXT         PRIMARY KEY
        CHECK (version_id ~ '^ddv_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                   VARCHAR(64)  NOT NULL,
    dataset_id                    TEXT         NOT NULL REFERENCES analytical_datasets(dataset_id),
    version_number                   INT          NOT NULL CHECK (version_number >= 1),
    schema_definition                   JSONB        NOT NULL,
    transformation_version                 VARCHAR(256) NOT NULL,
    source_checkpoint_ref                     VARCHAR(256) NOT NULL,
    residency_region                             VARCHAR(64)  NOT NULL,
    classification                                 VARCHAR(64)  NOT NULL,
    max_staleness_seconds                            BIGINT       NOT NULL CHECK (max_staleness_seconds > 0),
    status                                             VARCHAR(32)  NOT NULL DEFAULT 'Draft'
        CHECK (status IN ('Draft', 'Building', 'Validated', 'Published', 'Deprecated', 'Quarantined')),
    created_at                                           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                                             VARCHAR(128) NOT NULL,
    published_at                                             TIMESTAMPTZ,
    published_by                                               VARCHAR(128),
    UNIQUE (tenant_id, dataset_id, version_number),
    CHECK ((published_at IS NULL) = (published_by IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_dataset_versions_dataset ON dataset_versions(tenant_id, dataset_id);
CREATE INDEX IF NOT EXISTS idx_dataset_versions_status ON dataset_versions(tenant_id, status);

-- Snapshots: one materialization of a version's data. Immutable once
-- written; multiple snapshots may exist for a version while it's still
-- unpublished (rebuild-before-publish), never after Publish.
CREATE TABLE IF NOT EXISTS dataset_snapshots (
    snapshot_id              TEXT         PRIMARY KEY
        CHECK (snapshot_id ~ '^dsn_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                  VARCHAR(64)  NOT NULL,
    version_id                   TEXT         NOT NULL REFERENCES dataset_versions(version_id),
    watermark                      VARCHAR(256) NOT NULL,
    row_count                        BIGINT       NOT NULL CHECK (row_count >= 0),
    content_hash                       CHAR(64)     NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    built_at                             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    built_by                               VARCHAR(128) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_dataset_snapshots_version ON dataset_snapshots(tenant_id, version_id, built_at);

-- Partitions: one keyed slice of a snapshot. RebuildPartition always
-- creates a NEW row for a given key — reproducible rebuild, never an
-- in-place edit.
CREATE TABLE IF NOT EXISTS dataset_partitions (
    partition_id             TEXT         PRIMARY KEY
        CHECK (partition_id ~ '^dpt_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                  VARCHAR(64)  NOT NULL,
    version_id                   TEXT         NOT NULL REFERENCES dataset_versions(version_id),
    snapshot_id                    TEXT         NOT NULL REFERENCES dataset_snapshots(snapshot_id),
    partition_key                    VARCHAR(256) NOT NULL,
    row_count                          BIGINT       NOT NULL CHECK (row_count >= 0),
    content_hash                         CHAR(64)     NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    built_at                               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    built_by                                 VARCHAR(128) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_dataset_partitions_version ON dataset_partitions(tenant_id, version_id);
CREATE INDEX IF NOT EXISTS idx_dataset_partitions_key ON dataset_partitions(tenant_id, version_id, partition_key, built_at);

-- Data Product Certifications: sealed, immutable evidence produced at
-- PublishDatasetVersion time.
CREATE TABLE IF NOT EXISTS data_product_certifications (
    certification_id         TEXT         PRIMARY KEY
        CHECK (certification_id ~ '^ddc_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                   VARCHAR(64)  NOT NULL,
    version_id                    TEXT         NOT NULL UNIQUE REFERENCES dataset_versions(version_id),
    summary                          JSONB        NOT NULL,
    summary_sha256                      CHAR(64)     NOT NULL CHECK (summary_sha256 ~ '^[0-9a-f]{64}$'),
    certified_at                           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    certified_by                             VARCHAR(128) NOT NULL
);

-- Idempotency Keys: scoped per tenant from the start.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           VARCHAR(64)  NOT NULL,
    owner_scope           VARCHAR(32)  NOT NULL,
    principal_id             VARCHAR(128) NOT NULL,
    idempotency_key            VARCHAR(128) NOT NULL,
    operation                    VARCHAR(64)  NOT NULL,
    request_sha256                 VARCHAR(64)  NOT NULL,
    resource_id                      VARCHAR(128) NOT NULL,
    created_at                         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, owner_scope, principal_id, idempotency_key)
);

-- Outbox Events: transactional outbox for event emission.
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type        VARCHAR(64)  NOT NULL,
    aggregate_id            VARCHAR(128) NOT NULL,
    event_type                VARCHAR(64)  NOT NULL,
    payload                      JSONB        NOT NULL,
    tenant_id                      VARCHAR(64),
    created_at                       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at                       TIMESTAMPTZ,
    publish_attempts                     INT          NOT NULL DEFAULT 0,
    last_error                             TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished ON outbox_events(published_at) WHERE published_at IS NULL;

-- ── Row Level Security ──────────────────────────────────────────────────────

ALTER TABLE analytical_datasets ENABLE ROW LEVEL SECURITY;
ALTER TABLE analytical_datasets FORCE ROW LEVEL SECURITY;
CREATE POLICY analytical_datasets_tenant_isolation ON analytical_datasets
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE dataset_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE dataset_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY dataset_versions_read ON dataset_versions FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dataset_versions_insert ON dataset_versions FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dataset_versions_update ON dataset_versions FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dataset_versions_delete ON dataset_versions FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE dataset_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE dataset_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY dataset_snapshots_read ON dataset_snapshots FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dataset_snapshots_insert ON dataset_snapshots FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dataset_snapshots_update ON dataset_snapshots FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dataset_snapshots_delete ON dataset_snapshots FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE dataset_partitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE dataset_partitions FORCE ROW LEVEL SECURITY;
CREATE POLICY dataset_partitions_read ON dataset_partitions FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dataset_partitions_insert ON dataset_partitions FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dataset_partitions_update ON dataset_partitions FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dataset_partitions_delete ON dataset_partitions FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE data_product_certifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE data_product_certifications FORCE ROW LEVEL SECURITY;
CREATE POLICY data_product_certifications_read ON data_product_certifications FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY data_product_certifications_insert ON data_product_certifications FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY data_product_certifications_update ON data_product_certifications FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY data_product_certifications_delete ON data_product_certifications FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- ── Immutability ─────────────────────────────────────────────────────────────

CREATE OR REPLACE FUNCTION enforce_analytical_dataset_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'analytical dataset is immutable once created';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_analytical_dataset_immutability
    BEFORE UPDATE OR DELETE ON analytical_datasets
    FOR EACH ROW EXECUTE FUNCTION enforce_analytical_dataset_immutability();

CREATE OR REPLACE FUNCTION enforce_dataset_snapshot_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'dataset snapshot is immutable once written';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_dataset_snapshot_immutability
    BEFORE UPDATE OR DELETE ON dataset_snapshots
    FOR EACH ROW EXECUTE FUNCTION enforce_dataset_snapshot_immutability();

CREATE OR REPLACE FUNCTION enforce_dataset_partition_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'dataset partition is immutable once written';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_dataset_partition_immutability
    BEFORE UPDATE OR DELETE ON dataset_partitions
    FOR EACH ROW EXECUTE FUNCTION enforce_dataset_partition_immutability();

CREATE OR REPLACE FUNCTION enforce_certification_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'data product certification is sealed and immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_certification_immutability
    BEFORE UPDATE OR DELETE ON data_product_certifications
    FOR EACH ROW EXECUTE FUNCTION enforce_certification_immutability();

-- Dataset Versions: Deprecated/Quarantined are fully terminal (immutable
-- outright). Published is almost terminal — its CONTENT (schema,
-- transformation_version, checkpoint, every snapshot/partition beneath
-- it) can never change again, which is the real enforcement point for
-- "published dataset cannot be silently rebuilt in place" — but the
-- STATUS itself may still move forward exactly once, to Deprecated or
-- Quarantined (DeprecateDataset), and only that single column: any
-- update touching content alongside a Published row's status is refused.
CREATE OR REPLACE FUNCTION enforce_dataset_version_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'dataset version cannot be deleted';
    END IF;
    IF OLD.status IN ('Deprecated', 'Quarantined') THEN
        RAISE EXCEPTION 'dataset version % is % and immutable', OLD.version_id, OLD.status;
    END IF;
    IF OLD.status = 'Published' THEN
        IF NEW.status NOT IN ('Deprecated', 'Quarantined') THEN
            RAISE EXCEPTION 'dataset version % is Published; only Deprecated/Quarantined remain reachable', OLD.version_id;
        END IF;
        IF NEW.dataset_id <> OLD.dataset_id OR NEW.version_number <> OLD.version_number
            OR NEW.schema_definition <> OLD.schema_definition OR NEW.transformation_version <> OLD.transformation_version
            OR NEW.source_checkpoint_ref <> OLD.source_checkpoint_ref OR NEW.residency_region <> OLD.residency_region
            OR NEW.classification <> OLD.classification OR NEW.max_staleness_seconds <> OLD.max_staleness_seconds
            OR NEW.published_at <> OLD.published_at OR NEW.published_by <> OLD.published_by THEN
            RAISE EXCEPTION 'dataset version % is Published; only its status may change (to Deprecated/Quarantined)', OLD.version_id;
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'Draft' THEN
                IF NEW.status NOT IN ('Building', 'Validated', 'Quarantined') THEN
                    RAISE EXCEPTION 'invalid dataset version transition from Draft to %', NEW.status;
                END IF;
            WHEN 'Building' THEN
                IF NEW.status NOT IN ('Validated', 'Quarantined') THEN
                    RAISE EXCEPTION 'invalid dataset version transition from Building to %', NEW.status;
                END IF;
            WHEN 'Validated' THEN
                IF NEW.status NOT IN ('Published', 'Deprecated', 'Quarantined') THEN
                    RAISE EXCEPTION 'invalid dataset version transition from Validated to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown dataset version status %', OLD.status;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_dataset_version_lifecycle
    BEFORE UPDATE OR DELETE ON dataset_versions
    FOR EACH ROW EXECUTE FUNCTION enforce_dataset_version_lifecycle();
