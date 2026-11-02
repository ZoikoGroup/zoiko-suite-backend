-- 000005_org_unit_change_requests.up.sql
--
-- ORG-05 maker-checker for structural changes (ZS-SVC-I-001 s4.5): a reparent
-- that carries financial mappings is proposed here and applied only when a
-- different principal approves it.

CREATE TABLE IF NOT EXISTS org_unit_change_requests (
    request_id        UUID PRIMARY KEY,
    tenant_id         VARCHAR(255) NOT NULL,
    legal_entity_id   VARCHAR(255) NOT NULL,
    unit_id           UUID NOT NULL REFERENCES org_units(unit_id),
    command           VARCHAR(20)  NOT NULL,
    new_parent_unit_id UUID REFERENCES org_units(unit_id),
    effective_at      DATE NOT NULL,
    reason            TEXT NOT NULL,
    unit_version      BIGINT NOT NULL,
    impact            JSONB NOT NULL,
    status            VARCHAR(20)  NOT NULL DEFAULT 'PENDING',
    requested_by      VARCHAR(255) NOT NULL,
    requested_at      TIMESTAMP WITH TIME ZONE NOT NULL,
    decided_by        VARCHAR(255),
    decided_at        TIMESTAMP WITH TIME ZONE,
    decision_reason   TEXT NOT NULL DEFAULT '',
    failure_code      VARCHAR(50)  NOT NULL DEFAULT '',
    CONSTRAINT org_cr_command_valid CHECK (command IN ('REPARENT')),
    CONSTRAINT org_cr_status_valid  CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'FAILED')),
    -- A decided request names its decider and time; a pending one has neither.
    CONSTRAINT org_cr_decision_recorded CHECK (
        (status = 'PENDING' AND decided_by IS NULL AND decided_at IS NULL)
        OR (status <> 'PENDING' AND decided_by IS NOT NULL AND decided_at IS NOT NULL)),
    -- Segregation of duties, enforced by the database as well as the service:
    -- nobody approves a change they proposed.
    CONSTRAINT org_cr_no_self_approval CHECK (status <> 'APPROVED' OR decided_by <> requested_by)
);

-- One change awaiting a decision per unit, so approvers never face two
-- competing proposals for the same structure.
CREATE UNIQUE INDEX idx_org_cr_one_pending
    ON org_unit_change_requests (tenant_id, unit_id) WHERE status = 'PENDING';
CREATE INDEX idx_org_cr_entity_status ON org_unit_change_requests (tenant_id, legal_entity_id, status, requested_at);

ALTER TABLE org_unit_change_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_unit_change_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_org_unit_cr ON org_unit_change_requests FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
