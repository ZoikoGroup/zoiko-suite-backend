-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS corporate_actions (
    action_id        TEXT          NOT NULL,
    tenant_id        TEXT          NOT NULL,
    legal_entity_id  TEXT          NOT NULL,
    title            TEXT          NOT NULL,
    action_type      TEXT          NOT NULL,
    description      TEXT,
    resolution_id    TEXT,
    effective_date   DATE          NOT NULL,
    status           TEXT          NOT NULL DEFAULT 'PROPOSED',
    valuation_amount NUMERIC(20,4) NOT NULL DEFAULT 0,
    currency         TEXT          NOT NULL DEFAULT 'USD',
    executed_at      TIMESTAMPTZ,
    executed_by      TEXT,
    document_vault_id TEXT,
    effective_from   DATE          NOT NULL,
    effective_to     DATE,
    created_by       TEXT          NOT NULL,
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (action_id, tenant_id)
);

-- Row-Level Security
ALTER TABLE corporate_actions ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS actions_tenant_isolation ON corporate_actions;
CREATE POLICY actions_tenant_isolation ON corporate_actions
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_actions_tenant_entity ON corporate_actions (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_actions_action_type ON corporate_actions (action_type);
CREATE INDEX IF NOT EXISTS idx_actions_status ON corporate_actions (status);

COMMIT;
