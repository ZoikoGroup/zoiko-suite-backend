-- BNK-01's real masking/tokenization of the bank account identifier — the
-- doc's own "Security/KMS/Tokenization" dependency and its named
-- RotateAccountIdentifierToken command. Before this migration,
-- RegisterBankAccount/RotateAccountIdentifierToken simply stored whatever
-- string the caller sent in masked_account_number, with no real masking or
-- tokenization happening server-side — this table is the real token vault
-- row, from key-management-svc, that closes that gap.
--
-- WHY A SIDE TABLE, NOT A NEW COLUMN ON bank_accounts
-- Migration 000003's reject_bank_account_mutation trigger refuses ANY
-- update to a CLOSED account, and separately requires token_version to
-- increment by exactly 1 whenever masked_account_number/bank_identifier
-- change. A token column on bank_accounts would collide with both rules for
-- the same reason banking-connector-svc's migration 010
-- (bank_connection_account_tokens) chose a side table over a column on
-- bank_connections. This table needs no UPDATE of bank_accounts at all:
-- masked_account_number stays exactly what it always was — the display-safe
-- masked form, now computed server-side from the real value when
-- tokenization is enforced, still caller-supplied as before when it is not
-- — only the real token lives here.
--
-- READ PRECEDENCE
--   A row here with superseded_by IS NULL is this account's CURRENT token:
--   account_number_token is the real key-management-svc vault token.
--   bank_accounts.masked_account_number is ALWAYS the safe masked display
--   value (tokenized or not) — an ordinary read never needs this table.
--   The full number is only ever obtained by detokenizing
--   account_number_token, through the privileged GET
--   /v1/treasury/accounts/{id}/account-number route, under TOKEN_DETOKENIZE.
--
-- NO BACKFILL (see SPEC_DEVIATIONS and the task's own design note): unlike
-- banking-connector-svc/payee-banking-identity-svc, whose pre-tokenization
-- rows held a genuinely real raw value server-side, treasury-svc's
-- RegisterBankAccountRequest ORIGINAL contract only ever accepted
-- MaskedAccountNumber — an already-masked string the caller chose to send.
-- A legacy (non-tokenized) bank_accounts row therefore never had a real
-- account number on this service at all; there is nothing to retroactively
-- tokenize, so no cmd/tokenize-backfill-equivalent tool is added here.
--
-- Append-only + supersede, the same pattern migration 000003's
-- bank_account_ownership_evidence already uses: a rotation writes a NEW
-- token row and supersedes the old one, rather than overwriting it, so the
-- full token history survives for audit even though only the current
-- (superseded_by IS NULL) row is ever read by a live caller.
CREATE TABLE bank_account_identifier_tokens (
    token_id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bank_account_id       UUID NOT NULL REFERENCES bank_accounts (bank_account_id),
    tenant_id             VARCHAR(255) NOT NULL,
    account_number_token  TEXT NOT NULL CHECK (account_number_token <> ''),
    source                TEXT NOT NULL CHECK (source IN ('WRITE', 'ROTATION')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- DEFERRABLE INITIALLY DEFERRED: the store supersedes the PRIOR current
    -- row (pointing superseded_by at the NEW row's id) before inserting that
    -- new row, in the same transaction — so the referenced id does not exist
    -- yet at the time of the UPDATE. An IMMEDIATE foreign key would refuse
    -- that UPDATE; deferring the check to COMMIT lets both statements
    -- succeed together or not at all, exactly like any other same-
    -- transaction invariant in this codebase.
    superseded_by         UUID REFERENCES bank_account_identifier_tokens (token_id) DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX idx_bank_account_identifier_tokens_account ON bank_account_identifier_tokens (bank_account_id, created_at DESC);

-- Fast "is this account currently tokenized" / "give me the live token"
-- lookup, and a hard guarantee of at most one current token per account.
CREATE UNIQUE INDEX idx_bank_account_identifier_tokens_current
    ON bank_account_identifier_tokens (bank_account_id) WHERE superseded_by IS NULL;

ALTER TABLE bank_account_identifier_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_account_identifier_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON bank_account_identifier_tokens
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- The runtime connects as Postgres superuser (bypasses GRANT/REVOKE), so
-- only BEFORE UPDATE/DELETE triggers raising exceptions actually enforce
-- immutability here — same documented caveat as every other service in
-- this codebase's Banking domain.
CREATE OR REPLACE FUNCTION reject_identifier_token_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'bank_account_identifier_tokens rows are never deleted';
    END IF;
    IF NEW.bank_account_id IS DISTINCT FROM OLD.bank_account_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.account_number_token IS DISTINCT FROM OLD.account_number_token
        OR NEW.source IS DISTINCT FROM OLD.source
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'bank account identifier token % is append-only — only superseded_by may ever be set', OLD.token_id;
    END IF;
    IF OLD.superseded_by IS NOT NULL AND NEW.superseded_by IS DISTINCT FROM OLD.superseded_by THEN
        RAISE EXCEPTION 'bank account identifier token % is already superseded', OLD.token_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_identifier_token_mutation
    BEFORE UPDATE OR DELETE ON bank_account_identifier_tokens
    FOR EACH ROW EXECUTE FUNCTION reject_identifier_token_mutation();
