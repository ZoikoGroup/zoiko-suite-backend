-- Initial schema for Intercompany Accounting Service (intercompany-accounting-svc)

CREATE TABLE IF NOT EXISTS intercompany_entries (
    intercompany_entry_id  UUID PRIMARY KEY,
    tenant_id              VARCHAR(255) NOT NULL,
    source_legal_entity_id VARCHAR(255) NOT NULL,
    target_legal_entity_id VARCHAR(255) NOT NULL,
    source_journal_id      UUID NOT NULL,
    target_journal_id      UUID,
    amount                 NUMERIC(18,4) NOT NULL CHECK (amount > 0),
    currency_code          VARCHAR(10) NOT NULL,
    match_status           VARCHAR(50) NOT NULL, -- 'UNMATCHED', 'MATCHED', 'MISMATCH'
    mismatch_reason        TEXT,
    created_at             TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at             TIMESTAMP WITH TIME ZONE NOT NULL,
    CONSTRAINT chk_different_entities CHECK (source_legal_entity_id <> target_legal_entity_id)
);

-- Enable Row-Level Security
ALTER TABLE intercompany_entries ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policy
CREATE POLICY tenant_isolation_policy ON intercompany_entries FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance Indexes
CREATE INDEX idx_intercompany_tenant_entities ON intercompany_entries (tenant_id, source_legal_entity_id, target_legal_entity_id);
CREATE INDEX idx_intercompany_source_journal ON intercompany_entries (source_journal_id);
