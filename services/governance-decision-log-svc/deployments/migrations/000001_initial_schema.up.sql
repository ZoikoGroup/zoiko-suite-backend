-- governance_decisions is an append-only evidence table.
-- No UPDATE, no DELETE — ever. Corrections, if ever needed, happen by
-- writing a new decision record that references the original via
-- evaluation_context, never by mutating this row.
CREATE TABLE IF NOT EXISTS governance_decisions (
    decision_id        VARCHAR(64)  PRIMARY KEY,
    tenant_id          VARCHAR(64)  NOT NULL,
    legal_entity_id    VARCHAR(64)  NOT NULL,
    actor_id           VARCHAR(64)  NOT NULL,
    action_type        VARCHAR(128) NOT NULL,
    outcome            VARCHAR(32)  NOT NULL,
    rule_basis         VARCHAR(256) NOT NULL,
    evaluation_context JSONB,
    correlation_id     VARCHAR(64)  NOT NULL,
    decided_at         TIMESTAMPTZ  NOT NULL,
    stored_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Supports the five-filter query contract (actor, entity, action, rule
-- basis, time range) required by 03-microservices.md §8.7.
CREATE INDEX IF NOT EXISTS idx_governance_decisions_tenant_entity
    ON governance_decisions (tenant_id, legal_entity_id);

CREATE INDEX IF NOT EXISTS idx_governance_decisions_actor
    ON governance_decisions (actor_id);

CREATE INDEX IF NOT EXISTS idx_governance_decisions_action_type
    ON governance_decisions (action_type);

CREATE INDEX IF NOT EXISTS idx_governance_decisions_rule_basis
    ON governance_decisions (rule_basis);

CREATE INDEX IF NOT EXISTS idx_governance_decisions_decided_at
    ON governance_decisions (decided_at);