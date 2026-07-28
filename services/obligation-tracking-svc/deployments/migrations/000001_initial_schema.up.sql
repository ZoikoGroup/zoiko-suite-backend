-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS obligations (
    obligation_id    TEXT        NOT NULL,
    tenant_id        TEXT        NOT NULL,
    legal_entity_id  TEXT        NOT NULL,
    source_type      TEXT        NOT NULL,
    source_id        TEXT        NOT NULL,
    title            TEXT        NOT NULL,
    description      TEXT,
    obligation_type  TEXT        NOT NULL,
    risk_level       TEXT        NOT NULL DEFAULT 'MEDIUM',
    status           TEXT        NOT NULL DEFAULT 'PENDING',
    due_date         DATE        NOT NULL,
    assigned_to      TEXT,
    fulfilled_at     TIMESTAMPTZ,
    fulfilled_by     TEXT,
    fulfillment_note TEXT,
    effective_from   DATE        NOT NULL,
    effective_to     DATE,
    created_by       TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (obligation_id, tenant_id)
);

-- Row-Level Security
ALTER TABLE obligations ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS obligations_tenant_isolation ON obligations;
CREATE POLICY obligations_tenant_isolation ON obligations
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_obligations_tenant_entity ON obligations (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_obligations_source ON obligations (source_type, source_id);
CREATE INDEX IF NOT EXISTS idx_obligations_due_date ON obligations (due_date);
CREATE INDEX IF NOT EXISTS idx_obligations_status ON obligations (status);

COMMIT;
