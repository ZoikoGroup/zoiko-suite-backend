-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS filing_drafts (
    draft_id               TEXT        NOT NULL,
    tenant_id              TEXT        NOT NULL,
    legal_entity_id        TEXT        NOT NULL,
    jurisdiction_id        TEXT        NOT NULL,
    filing_type            TEXT        NOT NULL DEFAULT 'VAT',
    period_key             TEXT        NOT NULL,
    due_date               DATE        NOT NULL,
    payload_data           TEXT        NOT NULL DEFAULT '{}',
    evidence_manifest_ref  TEXT        NOT NULL DEFAULT '',
    validation_status      TEXT        NOT NULL DEFAULT 'DRAFT',
    block_reasons          TEXT        NOT NULL DEFAULT '',
    notes                  TEXT        NOT NULL DEFAULT '',
    created_by             TEXT        NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (draft_id, tenant_id),
    CONSTRAINT uk_filing_draft_period UNIQUE (tenant_id, legal_entity_id, jurisdiction_id, filing_type, period_key)
);

-- Row-Level Security: all queries must carry app.tenant_id
ALTER TABLE filing_drafts ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS filing_drafts_tenant_isolation ON filing_drafts;
CREATE POLICY filing_drafts_tenant_isolation ON filing_drafts
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_filing_drafts_tenant_entity ON filing_drafts (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_filing_drafts_jurisdiction  ON filing_drafts (jurisdiction_id);
CREATE INDEX IF NOT EXISTS idx_filing_drafts_type          ON filing_drafts (filing_type);
CREATE INDEX IF NOT EXISTS idx_filing_drafts_status        ON filing_drafts (validation_status);

COMMIT;
