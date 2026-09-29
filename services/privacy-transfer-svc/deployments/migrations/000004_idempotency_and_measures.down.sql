-- 000004_idempotency_and_measures.down.sql

DROP POLICY IF EXISTS tenant_isolation_policy ON transfer_idempotency_keys;
DROP TABLE IF EXISTS transfer_idempotency_keys;

ALTER TABLE transfer_assessments
    DROP COLUMN IF EXISTS government_access_risk,
    DROP COLUMN IF EXISTS technical_measures,
    DROP COLUMN IF EXISTS organizational_measures;
