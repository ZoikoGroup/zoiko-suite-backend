-- Migration: 000001_initial_schema.up.sql
-- Owned records for intercompany-accounting-svc.
-- Tenant-isolated via Postgres Row-Level Security AND explicit tenant_id
-- filters in every store query — RLS alone is insufficient given this
-- platform connects as a Postgres superuser (found via general-ledger-svc's
-- CI failure; see internal/store/pg_store.go's package doc comment).
--
-- source_legal_entity_id and target_legal_entity_id are two distinct
-- columns, never collapsed into one net position — the critical constraint
-- from 03-microservices.md §10.6.

CREATE TABLE intercompany_entries (
    intercompany_entry_id    UUID PRIMARY KEY,
    tenant_id                UUID NOT NULL,
    source_legal_entity_id   UUID NOT NULL,
    target_legal_entity_id   UUID NOT NULL,

    source_journal_entry_id  UUID NOT NULL,
    target_journal_entry_id  UUID,

    amount                   NUMERIC(18,2) NOT NULL,
    currency_code            VARCHAR(3) NOT NULL,
    description              VARCHAR(500) NOT NULL,
    match_status             VARCHAR(20) NOT NULL,

    mismatch_reason          VARCHAR(500),

    created_by_principal_id  VARCHAR(255) NOT NULL,
    matched_by_principal_id  VARCHAR(255),
    correlation_id           VARCHAR(255) NOT NULL,
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    matched_at               TIMESTAMP WITH TIME ZONE,
    mismatched_at            TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_intercompany_entries_tenant ON intercompany_entries (tenant_id);
CREATE INDEX idx_intercompany_entries_status ON intercompany_entries (match_status);
CREATE INDEX idx_intercompany_entries_source_entity ON intercompany_entries (tenant_id, source_legal_entity_id);
CREATE INDEX idx_intercompany_entries_target_entity ON intercompany_entries (tenant_id, target_legal_entity_id);

ALTER TABLE intercompany_entries ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON intercompany_entries
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);
