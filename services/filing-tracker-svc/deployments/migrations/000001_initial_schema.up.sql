-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS filing_requirements (
    filing_id              TEXT        NOT NULL,
    tenant_id              TEXT        NOT NULL,
    legal_entity_id        TEXT        NOT NULL,
    jurisdiction_id        TEXT        NOT NULL,
    filing_authority       TEXT        NOT NULL,
    filing_type            TEXT        NOT NULL DEFAULT 'VAT',
    period_key             TEXT        NOT NULL,
    due_date               DATE        NOT NULL,
    status                 TEXT        NOT NULL DEFAULT 'SCHEDULED',
    submission_reference   TEXT,
    submitted_at           TIMESTAMPTZ,
    submitted_by           TEXT,
    confirmation_reference TEXT,
    confirmed_at           TIMESTAMPTZ,
    rejection_reason       TEXT        NOT NULL DEFAULT '',
    notes                  TEXT        NOT NULL DEFAULT '',
    created_by             TEXT        NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (filing_id, tenant_id),
    CONSTRAINT uk_filing_req_period UNIQUE (tenant_id, legal_entity_id, jurisdiction_id, filing_type, period_key)
);

-- Row-Level Security: all queries must carry app.tenant_id
ALTER TABLE filing_requirements ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS filing_requirements_tenant_isolation ON filing_requirements;
CREATE POLICY filing_requirements_tenant_isolation ON filing_requirements
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_filing_req_tenant_entity ON filing_requirements (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_filing_req_jurisdiction  ON filing_requirements (jurisdiction_id);
CREATE INDEX IF NOT EXISTS idx_filing_req_authority     ON filing_requirements (filing_authority);
CREATE INDEX IF NOT EXISTS idx_filing_req_status        ON filing_requirements (status);
CREATE INDEX IF NOT EXISTS idx_filing_req_due_date      ON filing_requirements (due_date);

COMMIT;
