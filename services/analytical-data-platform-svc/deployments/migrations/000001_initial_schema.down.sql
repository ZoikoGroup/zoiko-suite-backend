-- Down migration for analytical-data-platform-svc (DATA-04)
-- Migration: 000001_initial_schema.down.sql

DROP TRIGGER IF EXISTS trigger_dataset_version_lifecycle ON dataset_versions;
DROP FUNCTION IF EXISTS enforce_dataset_version_lifecycle();

DROP TRIGGER IF EXISTS trigger_certification_immutability ON data_product_certifications;
DROP FUNCTION IF EXISTS enforce_certification_immutability();

DROP TRIGGER IF EXISTS trigger_dataset_partition_immutability ON dataset_partitions;
DROP FUNCTION IF EXISTS enforce_dataset_partition_immutability();

DROP TRIGGER IF EXISTS trigger_dataset_snapshot_immutability ON dataset_snapshots;
DROP FUNCTION IF EXISTS enforce_dataset_snapshot_immutability();

DROP TRIGGER IF EXISTS trigger_analytical_dataset_immutability ON analytical_datasets;
DROP FUNCTION IF EXISTS enforce_analytical_dataset_immutability();

DROP INDEX IF EXISTS idx_outbox_events_unpublished;
DROP INDEX IF EXISTS idx_dataset_partitions_key;
DROP INDEX IF EXISTS idx_dataset_partitions_version;
DROP INDEX IF EXISTS idx_dataset_snapshots_version;
DROP INDEX IF EXISTS idx_dataset_versions_status;
DROP INDEX IF EXISTS idx_dataset_versions_dataset;

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
DROP POLICY IF EXISTS data_product_certifications_delete ON data_product_certifications;
DROP POLICY IF EXISTS data_product_certifications_update ON data_product_certifications;
DROP POLICY IF EXISTS data_product_certifications_insert ON data_product_certifications;
DROP POLICY IF EXISTS data_product_certifications_read ON data_product_certifications;
DROP POLICY IF EXISTS dataset_partitions_delete ON dataset_partitions;
DROP POLICY IF EXISTS dataset_partitions_update ON dataset_partitions;
DROP POLICY IF EXISTS dataset_partitions_insert ON dataset_partitions;
DROP POLICY IF EXISTS dataset_partitions_read ON dataset_partitions;
DROP POLICY IF EXISTS dataset_snapshots_delete ON dataset_snapshots;
DROP POLICY IF EXISTS dataset_snapshots_update ON dataset_snapshots;
DROP POLICY IF EXISTS dataset_snapshots_insert ON dataset_snapshots;
DROP POLICY IF EXISTS dataset_snapshots_read ON dataset_snapshots;
DROP POLICY IF EXISTS dataset_versions_delete ON dataset_versions;
DROP POLICY IF EXISTS dataset_versions_update ON dataset_versions;
DROP POLICY IF EXISTS dataset_versions_insert ON dataset_versions;
DROP POLICY IF EXISTS dataset_versions_read ON dataset_versions;
DROP POLICY IF EXISTS analytical_datasets_tenant_isolation ON analytical_datasets;

ALTER TABLE outbox_events DISABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys DISABLE ROW LEVEL SECURITY;
ALTER TABLE data_product_certifications DISABLE ROW LEVEL SECURITY;
ALTER TABLE dataset_partitions DISABLE ROW LEVEL SECURITY;
ALTER TABLE dataset_snapshots DISABLE ROW LEVEL SECURITY;
ALTER TABLE dataset_versions DISABLE ROW LEVEL SECURITY;
ALTER TABLE analytical_datasets DISABLE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS data_product_certifications;
DROP TABLE IF EXISTS dataset_partitions;
DROP TABLE IF EXISTS dataset_snapshots;
DROP TABLE IF EXISTS dataset_versions;
DROP TABLE IF EXISTS analytical_datasets;
