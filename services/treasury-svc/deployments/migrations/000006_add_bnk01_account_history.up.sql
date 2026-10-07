-- BNK-01 account version history — Wave 10a of the gap-remediation plan.
--
-- The doc's Authoritative-ownership row for BankAccount explicitly names
-- "account-version history" as a field, and Invariant #1 requires bank
-- account ownership/operational status to be "historically
-- reconstructable." Before this migration, every mutating BNK-01 command
-- (AmendBankAccountMetadata, ChangeOperationalUse, Suspend/Reactivate/
-- Close, RotateAccountIdentifierToken) overwrote bank_accounts in place
-- with no trace of the prior value ever kept.
--
-- Mirrors bank_account_ownership_evidence (migration 000003) exactly:
-- append-only rows, a superseded_by self-FK set exactly once, and a
-- BEFORE UPDATE/DELETE trigger enforcing both. This is the established
-- "history" idiom in this service — not a new one.
CREATE TABLE bank_account_history (
    history_id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bank_account_id            UUID NOT NULL REFERENCES bank_accounts (bank_account_id),
    tenant_id                  VARCHAR(255) NOT NULL,

    -- Snapshot of every field on bank_accounts that a BNK-01 command can
    -- change. legal_entity_id/created_at/created_by_principal_id are
    -- deliberately excluded — migration 000003's own
    -- reject_bank_account_mutation trigger already makes those immutable,
    -- so there is nothing to version.
    account_name               TEXT NOT NULL,
    masked_account_number      TEXT NOT NULL,
    bank_identifier             TEXT NOT NULL,
    account_status              TEXT NOT NULL,
    branch_ref                   TEXT NOT NULL,
    country                       TEXT NOT NULL,
    account_type                 TEXT NOT NULL,
    requested_operational_use    TEXT NOT NULL,
    token_version                 INT NOT NULL,

    changed_by_principal_id    TEXT NOT NULL,
    effective_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    superseded_by                 UUID REFERENCES bank_account_history (history_id)
);

CREATE INDEX idx_bank_account_history_account ON bank_account_history (bank_account_id, effective_at DESC);

ALTER TABLE bank_account_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_account_history FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON bank_account_history
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Same enforcement shape as reject_ownership_evidence_mutation: every
-- field is append-only except superseded_by, which may be set exactly
-- once (NULL -> a value), never changed again, never deleted.
CREATE OR REPLACE FUNCTION reject_account_history_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'bank_account_history rows are never deleted';
    END IF;
    IF NEW.bank_account_id IS DISTINCT FROM OLD.bank_account_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.account_name IS DISTINCT FROM OLD.account_name
        OR NEW.masked_account_number IS DISTINCT FROM OLD.masked_account_number
        OR NEW.bank_identifier IS DISTINCT FROM OLD.bank_identifier
        OR NEW.account_status IS DISTINCT FROM OLD.account_status
        OR NEW.branch_ref IS DISTINCT FROM OLD.branch_ref
        OR NEW.country IS DISTINCT FROM OLD.country
        OR NEW.account_type IS DISTINCT FROM OLD.account_type
        OR NEW.requested_operational_use IS DISTINCT FROM OLD.requested_operational_use
        OR NEW.token_version IS DISTINCT FROM OLD.token_version
        OR NEW.changed_by_principal_id IS DISTINCT FROM OLD.changed_by_principal_id
        OR NEW.effective_at IS DISTINCT FROM OLD.effective_at
    THEN
        RAISE EXCEPTION 'bank account history row % is append-only — only superseded_by may ever be set', OLD.history_id;
    END IF;
    IF OLD.superseded_by IS NOT NULL AND NEW.superseded_by IS DISTINCT FROM OLD.superseded_by THEN
        RAISE EXCEPTION 'bank account history row % is already superseded', OLD.history_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_account_history_mutation
    BEFORE UPDATE OR DELETE ON bank_account_history
    FOR EACH ROW EXECUTE FUNCTION reject_account_history_mutation();
