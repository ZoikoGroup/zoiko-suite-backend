DROP INDEX IF EXISTS idx_report_metric_definitions_code_idempotency;
ALTER TABLE report_metric_definitions
    DROP COLUMN IF EXISTS idempotency_key;
