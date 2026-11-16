-- Account-number tokens, beside bank_connections rather than inside it.
--
-- Security Standard §14 (tokenize where the raw value is unnecessary). A
-- token for a connection's account number lives here, one row per
-- connection, written either at create time (TOKENIZATION_ENFORCED=true) or
-- by the one-off backfill (cmd/tokenize-backfill) for rows written raw
-- before enforcement.
--
-- WHY A SIDE TABLE, NOT A NEW COLUMN ON bank_connections
-- Migration 003's trigger refuses EVERY update to a REVOKED connection, not
-- just changes to particular fields. A token column on bank_connections could
-- therefore never be backfilled for revoked connections without loosening
-- that trigger, and revoked rows still hold raw account numbers that need
-- protecting. This table needs no UPDATE of bank_connections at all: the
-- backfill only INSERTs here, the existing trigger stays exactly as it is,
-- and account_number is never written in place.
--
-- READ PRECEDENCE (the one rule every reader follows)
--   A row here means the connection is TOKENIZED: account_number_token is the
--   value, account_number_masked is its display form, and bank_connections.
--   account_number is never read as a bank account number for it.
--   No row here means a LEGACY raw value: bank_connections.account_number is
--   the raw number, exactly as before.
-- The discriminator is the presence of this row, never the shape of the
-- stored string.
--
-- account_number_masked is the masked display form ("****…6819": the length
-- of the raw value, last 4 visible), computed from the raw number when the
-- token is written. List and get responses show it directly, so a masked view
-- never needs the vault, and TOKEN_DETOKENIZE stays with the readers who
-- genuinely need the full number.
--
-- bank_connections.account_number is left in place: for a backfilled row it
-- still holds the raw value. Dropping or overwriting it is a separate, later
-- decision (SPEC_DEVIATIONS.md G6).
CREATE TABLE IF NOT EXISTS bank_connection_account_tokens (
    connection_id         VARCHAR(64) PRIMARY KEY REFERENCES bank_connections(connection_id),
    tenant_id             VARCHAR(64) NOT NULL,
    account_number_token  TEXT        NOT NULL CHECK (account_number_token <> ''),
    account_number_masked TEXT        NOT NULL,
    source                TEXT        NOT NULL CHECK (source IN ('WRITE', 'BACKFILL')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Append-only: a token row is set once. Repointing it would silently change
-- which bank account every reader of this connection sees.
CREATE OR REPLACE FUNCTION reject_account_token_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'bank_connection_account_tokens rows are append-only: % refused', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_reject_account_token_mutation ON bank_connection_account_tokens;
CREATE TRIGGER trg_reject_account_token_mutation
    BEFORE UPDATE OR DELETE ON bank_connection_account_tokens
    FOR EACH ROW EXECUTE FUNCTION reject_account_token_mutation();

-- Same tenant policy as bank_connections (002_add_rls.sql).
ALTER TABLE bank_connection_account_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_connection_account_tokens FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON bank_connection_account_tokens;
CREATE POLICY tenant_isolation_policy ON bank_connection_account_tokens
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
