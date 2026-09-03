-- Reverts 000009 back to a single unpartitioned access_decision_log.
--
-- Copies every row out of every ATTACHED partition first. Rows in partitions a
-- retention run has already DETACHED are NOT recovered — a detached partition
-- is no longer part of this table, and this migration has no way to know which
-- loose tables were once part of it or whether they have since been archived
-- and dropped. Check access_decision_log_retention_status and any detached
-- access_decision_log_* tables before running this.
BEGIN;

ALTER TABLE access_decision_log RENAME TO access_decision_log_partitioned;

CREATE TABLE access_decision_log (
    access_decision_id       UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id             TEXT         NOT NULL,
    legal_entity_id          UUID         NOT NULL,
    action_type              VARCHAR(128) NOT NULL,
    decision_outcome         VARCHAR(16)  NOT NULL,
    decision_basis           TEXT         NOT NULL,
    tenant_id                UUID,
    correlation_id           TEXT,
    decided_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

INSERT INTO access_decision_log (
    access_decision_id, principal_id, legal_entity_id, action_type,
    decision_outcome, decision_basis, tenant_id, correlation_id, decided_at)
SELECT
    access_decision_id, principal_id, legal_entity_id, action_type,
    decision_outcome, decision_basis, tenant_id, correlation_id, decided_at
FROM access_decision_log_partitioned;

DROP TABLE access_decision_log_partitioned CASCADE;

CREATE INDEX idx_access_decision_log_principal ON access_decision_log (principal_id, decided_at DESC);
CREATE INDEX idx_access_decision_log_entity    ON access_decision_log (legal_entity_id, decided_at DESC);
CREATE INDEX idx_access_decision_log_tenant    ON access_decision_log (tenant_id, decided_at DESC);

ALTER TABLE access_decision_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_decision_log FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON access_decision_log
    FOR ALL
    USING      (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

DROP VIEW     IF EXISTS access_decision_log_retention_status;
DROP FUNCTION IF EXISTS detach_access_decision_log_partitions_before(DATE);
DROP FUNCTION IF EXISTS create_access_decision_log_partition(DATE);

COMMIT;
