-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS clauses (
    clause_id       TEXT        NOT NULL,
    tenant_id       TEXT        NOT NULL,
    legal_entity_id TEXT        NOT NULL,
    title           TEXT        NOT NULL,
    category        TEXT        NOT NULL,
    body            TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'DRAFT',
    version         INTEGER     NOT NULL DEFAULT 1,
    jurisdiction_id TEXT        NOT NULL,
    effective_from  DATE        NOT NULL,
    effective_to    DATE,
    created_by      TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (clause_id, tenant_id)
);

CREATE TABLE IF NOT EXISTS contract_templates (
    template_id     TEXT        NOT NULL,
    tenant_id       TEXT        NOT NULL,
    legal_entity_id TEXT        NOT NULL,
    title           TEXT        NOT NULL,
    contract_type   TEXT        NOT NULL,
    description     TEXT,
    clause_ids      TEXT[]      NOT NULL DEFAULT '{}',
    status          TEXT        NOT NULL DEFAULT 'DRAFT',
    version         INTEGER     NOT NULL DEFAULT 1,
    jurisdiction_id TEXT        NOT NULL,
    effective_from  DATE        NOT NULL,
    effective_to    DATE,
    created_by      TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (template_id, tenant_id)
);

-- Row-Level Security
ALTER TABLE clauses ENABLE ROW LEVEL SECURITY;
ALTER TABLE contract_templates ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS clauses_tenant_isolation ON clauses;
CREATE POLICY clauses_tenant_isolation ON clauses
    USING (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS templates_tenant_isolation ON contract_templates;
CREATE POLICY templates_tenant_isolation ON contract_templates
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_clauses_tenant_entity ON clauses (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_clauses_category ON clauses (category);
CREATE INDEX IF NOT EXISTS idx_templates_tenant_entity ON contract_templates (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_templates_contract_type ON contract_templates (contract_type);

COMMIT;
