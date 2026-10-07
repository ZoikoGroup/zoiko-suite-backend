-- Down migration for data-ingestion-svc (DATA-01)
-- Migration: 000001_initial_schema.down.sql

DROP TRIGGER IF EXISTS trigger_ingestion_run_updated_at ON ingestion_runs;
DROP FUNCTION IF EXISTS update_ingestion_run_updated_at();

DROP TRIGGER IF EXISTS trigger_run_lifecycle ON ingestion_runs;
DROP FUNCTION IF EXISTS enforce_run_lifecycle();

DROP TRIGGER IF EXISTS trigger_checkpoint_no_delete ON source_checkpoints;
DROP FUNCTION IF EXISTS enforce_checkpoint_no_delete();

DROP TRIGGER IF EXISTS trigger_quarantine_immutability ON quarantine_items;
DROP FUNCTION IF EXISTS enforce_quarantine_immutability();

DROP TRIGGER IF EXISTS trigger_landed_record_immutability ON landed_records;
DROP FUNCTION IF EXISTS enforce_landed_record_immutability();

DROP TRIGGER IF EXISTS trigger_landing_immutability ON landing_objects;
DROP FUNCTION IF EXISTS enforce_landing_immutability();

DROP INDEX IF EXISTS idx_outbox_events_unpublished;
DROP INDEX IF EXISTS idx_landed_records_landing;
DROP INDEX IF EXISTS idx_quarantine_items_tenant_source;
DROP INDEX IF EXISTS idx_quarantine_items_run;
DROP INDEX IF EXISTS idx_landing_objects_tenant_source;
DROP INDEX IF EXISTS idx_landing_objects_run;
DROP INDEX IF EXISTS idx_source_checkpoints_tenant_source;
DROP INDEX IF EXISTS idx_ingestion_runs_tenant_status;
DROP INDEX IF EXISTS idx_ingestion_runs_tenant_source;

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
DROP POLICY IF EXISTS landed_records_tenant_isolation ON landed_records;
DROP POLICY IF EXISTS quarantine_items_tenant_isolation ON quarantine_items;
DROP POLICY IF EXISTS landing_objects_tenant_isolation ON landing_objects;
DROP POLICY IF EXISTS source_checkpoints_tenant_isolation ON source_checkpoints;
DROP POLICY IF EXISTS ingestion_runs_tenant_isolation ON ingestion_runs;

ALTER TABLE outbox_events DISABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys DISABLE ROW LEVEL SECURITY;
ALTER TABLE landed_records DISABLE ROW LEVEL SECURITY;
ALTER TABLE quarantine_items DISABLE ROW LEVEL SECURITY;
ALTER TABLE landing_objects DISABLE ROW LEVEL SECURITY;
ALTER TABLE source_checkpoints DISABLE ROW LEVEL SECURITY;
ALTER TABLE ingestion_runs DISABLE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS landed_records;
DROP TABLE IF EXISTS quarantine_items;
DROP TABLE IF EXISTS landing_objects;
DROP TABLE IF EXISTS source_checkpoints;
DROP TABLE IF EXISTS ingestion_runs;