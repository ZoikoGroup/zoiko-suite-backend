-- BNK-09 Treasury Transfer: replaces the old `transfers` table/
-- ExecuteTransfer wholesale. The prior implementation only moved two
-- internal cash_balances rows and called nothing external — no approval
-- workflow, no real payment execution, no intercompany consequence. This
-- is a different (correct) operation with the same name, not an
-- extension, so the old table is dropped rather than kept alongside it.
--
-- Real flow: CreateTreasuryTransfer (maker) -> ApproveTreasuryTransfer
-- (checker) -> ExecuteTreasuryTransfer drives a resumable saga: submit via
-- payment-initiation-adapter-svc (BNK-06), then — only for cross-entity
-- transfers — post to general-ledger-svc and pair via
-- intercompany-accounting-svc. cash_balances mutation is removed from the
-- transfer path entirely; the payment/ledger/intercompany services now own
-- the real consequence of a transfer.
DROP TABLE IF EXISTS transfers CASCADE;

CREATE TABLE treasury_transfers (
    transfer_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               VARCHAR(255) NOT NULL,
    source_bank_account_id  UUID NOT NULL,
    target_bank_account_id  UUID NOT NULL,
    amount                  NUMERIC(20, 4) NOT NULL,
    currency_code           VARCHAR(3) NOT NULL,
    correlation_id          VARCHAR(255) NOT NULL DEFAULT '',
    is_cross_entity         BOOLEAN NOT NULL DEFAULT FALSE,

    -- sha256 hex of "amount|currency_code|source_bank_account_id|
    -- target_bank_account_id" as it stood at CreateTreasuryTransfer time.
    -- ApproveTreasuryTransfer recomputes it from the current row and
    -- refuses to approve on any mismatch — defense in depth against the
    -- protected fields having been altered between creation and approval.
    protected_field_hash    VARCHAR(64) NOT NULL,

    status                  VARCHAR(32) NOT NULL DEFAULT 'PENDING_APPROVAL',
    maker_principal_id      VARCHAR(128) NOT NULL,
    checker_principal_id    VARCHAR(128) NOT NULL DEFAULT '',
    reject_reason           TEXT NOT NULL DEFAULT '',

    payment_attempt_id      VARCHAR(64) NOT NULL DEFAULT '',
    source_journal_id       VARCHAR(64) NOT NULL DEFAULT '',
    intercompany_entry_id   VARCHAR(64) NOT NULL DEFAULT '',

    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_treasury_transfers_tenant_correlation
    ON treasury_transfers (tenant_id, correlation_id) WHERE correlation_id <> '';

ALTER TABLE treasury_transfers ENABLE ROW LEVEL SECURITY;
ALTER TABLE treasury_transfers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON treasury_transfers
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- COMPLETED and REJECTED are terminal — no further mutation, ever. A
-- checker approving must differ from the maker who created the transfer;
-- this DB-level check backs up the app-layer fetch-then-compare so a
-- direct-write bypass can't create a self-approved transfer either.
CREATE OR REPLACE FUNCTION reject_terminal_transfer_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'treasury_transfers rows are never deleted';
    END IF;
    IF OLD.status IN ('COMPLETED', 'REJECTED') THEN
        RAISE EXCEPTION 'treasury transfer % is % and can no longer be modified', OLD.transfer_id, OLD.status;
    END IF;
    IF NEW.checker_principal_id <> '' AND NEW.checker_principal_id = NEW.maker_principal_id THEN
        RAISE EXCEPTION 'treasury transfer % checker cannot be the same principal as the maker', OLD.transfer_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_terminal_transfer_mutation
    BEFORE UPDATE OR DELETE ON treasury_transfers
    FOR EACH ROW EXECUTE FUNCTION reject_terminal_transfer_mutation();
