-- Account-identifier tokens, beside payee_destinations rather than inside it.
--
-- Security Standard §14 (tokenize where the raw value is unnecessary). A
-- token for a destination's account identifier lives here, one row per
-- destination, written either at propose time (TOKENIZATION_ENFORCED=true) or
-- by the one-off backfill (cmd/tokenize-backfill) for destinations proposed
-- raw before enforcement.
--
-- WHY A SIDE TABLE, NOT A NEW COLUMN ON payee_destinations
-- 000002's trigger (reject_payee_destination_mutation) refuses ANY update to
-- a SUSPENDED or SUPERSEDED destination, whatever column it touches, and
-- refuses any change to account_identifier on every row. A token column on
-- payee_destinations could therefore never be backfilled for terminal rows
-- — which still hold raw identifiers — without weakening that trigger. Here
-- the backfill only INSERTs; payee_destinations is never updated, the trigger
-- is untouched, and account_identifier is never overwritten in place.
--
-- READ PRECEDENCE (the one rule every reader follows)
--   A row here means the destination is TOKENIZED: account_identifier_token
--   is the value, and payee_destinations.account_identifier is never served
--   as an account identifier for it — a privileged reader gets the
--   detokenized value, everyone else gets it blanked (account_last4 always
--   remains, computed from the raw value at propose time).
--   No row here means a LEGACY raw value: account_identifier is the raw
--   identifier, exactly as before.
-- The discriminator is the presence of this row, never the shape of the
-- stored string.
--
-- payee_destinations.account_identifier is left in place: for a backfilled
-- row it still holds the raw value (000002 forbids changing it, and dropping
-- the column is a separate, later decision — SPEC_DEVIATIONS.md B12).
CREATE TABLE IF NOT EXISTS payee_destination_identifier_tokens (
    destination_id           UUID        PRIMARY KEY REFERENCES payee_destinations(destination_id),
    tenant_id                UUID        NOT NULL,
    account_identifier_token TEXT        NOT NULL CHECK (account_identifier_token <> ''),
    source                   TEXT        NOT NULL CHECK (source IN ('WRITE', 'BACKFILL')),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Append-only, with 000002's own append-only function: a token row is set
-- once, because repointing it would silently change which bank account a
-- payment to this destination reaches.
DROP TRIGGER IF EXISTS trg_reject_identifier_token_mutation ON payee_destination_identifier_tokens;
CREATE TRIGGER trg_reject_identifier_token_mutation
    BEFORE UPDATE OR DELETE ON payee_destination_identifier_tokens
    FOR EACH ROW EXECUTE FUNCTION reject_payee_event_mutation();

-- 000003's tenant predicate, without its NULL-tenant (platform) branch: a
-- token is always bound to a tenant (the vault refuses one without).
ALTER TABLE payee_destination_identifier_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE payee_destination_identifier_tokens FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON payee_destination_identifier_tokens;
CREATE POLICY tenant_isolation ON payee_destination_identifier_tokens
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
