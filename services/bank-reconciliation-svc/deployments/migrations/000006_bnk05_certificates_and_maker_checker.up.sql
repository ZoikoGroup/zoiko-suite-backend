-- BNK-05: maker-checker manual matching + reconciliation_certificates.
--
-- Adds an OPTIONAL stricter path alongside the existing single-actor
-- MatchStatementLine (kept unchanged for backward compatibility, e.g.
-- automated/system reconciliation callers that don't need dual control):
-- ProposeMatch (maker) -> ConfirmMatch (checker, must differ from the
-- maker) -> MATCHED, via a new PENDING_CONFIRMATION intermediate status.
--
-- reconciliation_certificates gives CompleteStatement (previously just a
-- check-and-publish with no persisted record at all) a real, immutable
-- evidence row — one per (tenant, bank_account, statement_date).

ALTER TABLE statement_lines
    ADD COLUMN proposed_journal_id      UUID,
    ADD COLUMN proposed_by_principal_id VARCHAR(255),
    ADD COLUMN proposed_at              TIMESTAMP WITH TIME ZONE;

CREATE TABLE reconciliation_certificates (
    certificate_id            UUID PRIMARY KEY,
    tenant_id                 UUID NOT NULL,
    legal_entity_id           UUID NOT NULL,
    bank_account_id           UUID NOT NULL,
    statement_date            DATE NOT NULL,
    matched_line_count        INT NOT NULL,
    certified_by_principal_id VARCHAR(255) NOT NULL,
    certified_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id            VARCHAR(255) NOT NULL DEFAULT ''
);

-- One certificate per statement — CompleteStatement is idempotent: a
-- repeat call after certification returns the existing certificate
-- rather than raising a duplicate.
CREATE UNIQUE INDEX idx_reconciliation_certificates_statement
    ON reconciliation_certificates (tenant_id, bank_account_id, statement_date);

ALTER TABLE reconciliation_certificates ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_certificates FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON reconciliation_certificates
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

-- A certificate is evidence of a completed reconciliation — append-only,
-- never edited or withdrawn. The runtime connects as Postgres superuser
-- (see internal/store/pg_store.go's own package doc), so only a BEFORE
-- trigger actually enforces this.
CREATE OR REPLACE FUNCTION reject_certificate_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'reconciliation_certificates rows are append-only and can never be updated or deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_certificate_mutation
    BEFORE UPDATE OR DELETE ON reconciliation_certificates
    FOR EACH ROW EXECUTE FUNCTION reject_certificate_mutation();
