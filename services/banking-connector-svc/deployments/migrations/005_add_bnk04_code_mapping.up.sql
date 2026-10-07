-- BNK-04 real code-mapping dictionary. Before this migration,
-- NormalizeTransaction trusted a caller-supplied category/counterparty
-- guess outright — there was no dictionary to check it against and no way
-- for an unrecognized bank code to be automatically routed to a mapping
-- exception instead of silently accepted.

-- bank_code is the raw category/transaction-type code as it appears on the
-- statement line (e.g. a BAI2 type code) — evidence captured at ingest
-- time, immutable along with the rest of the line (see migration 004's
-- reject_statement_line_mutation, which already covers this table).
ALTER TABLE bank_statement_lines ADD COLUMN bank_code TEXT NOT NULL DEFAULT '';

-- bank_transaction_mappings: the tenant-scoped dictionary of known bank
-- codes to canonical categories. NormalizeTransaction looks up a line's
-- bank_code here; a miss means an unknown code, which is auto-routed to a
-- mapping exception rather than accepting any caller-supplied guess.
CREATE TABLE bank_transaction_mappings (
    mapping_id              VARCHAR(64) PRIMARY KEY DEFAULT gen_random_uuid()::text,
    tenant_id               VARCHAR(64) NOT NULL,
    bank_code                TEXT NOT NULL,
    category                 TEXT NOT NULL,
    created_by_principal_id  VARCHAR(128) NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_bank_transaction_mappings_tenant_code
    ON bank_transaction_mappings (tenant_id, bank_code);

ALTER TABLE bank_transaction_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_transaction_mappings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON bank_transaction_mappings
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
