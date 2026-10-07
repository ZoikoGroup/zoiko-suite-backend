-- BNK-01 Bank Account: the full identity/ownership/verification/
-- operational-status model, extending the pre-existing bank_accounts
-- table in place rather than creating a new service.
--
-- Backward compatibility: RegisterBankAccount keeps creating rows as
-- ACTIVE by default (unchanged from today) rather than starting them in
-- PENDING_VERIFICATION — changing that default would silently exclude
-- every newly-registered account from cash-position aggregation
-- (GetForecasts/GetEffectiveCash filter on account_status = 'ACTIVE')
-- until a caller remembers to call VerifyBankAccountOwnership, which
-- nothing in this codebase does automatically today. Ownership
-- verification is instead tracked as a genuinely orthogonal fact — see
-- bank_account_ownership_evidence below — exactly matching the doc's own
-- words: "ownership verification state is orthogonal and versioned."
-- account_status still gains the full DRAFT/PENDING_VERIFICATION leg as
-- valid values for callers that do want the stricter flow.

ALTER TABLE bank_accounts
    ADD COLUMN branch_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN country VARCHAR(2) NOT NULL DEFAULT '',
    ADD COLUMN account_type TEXT NOT NULL DEFAULT '',
    ADD COLUMN requested_operational_use TEXT NOT NULL DEFAULT '',
    ADD COLUMN correlation_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN created_by_principal_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN suspend_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN close_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN token_version INT NOT NULL DEFAULT 1;

ALTER TABLE bank_accounts
    ADD CONSTRAINT bank_accounts_status_valid
    CHECK (account_status IN ('DRAFT', 'PENDING_VERIFICATION', 'ACTIVE', 'SUSPENDED', 'CLOSED'));

-- Idempotency-by-correlation-id for CreateBankAccount — a retried request
-- returns the original row rather than creating a second account.
CREATE UNIQUE INDEX idx_bank_accounts_tenant_correlation
    ON bank_accounts (tenant_id, correlation_id) WHERE correlation_id <> '';

-- bank_account_ownership_evidence: append-only, superseded-not-mutated on
-- re-verification — the concrete form of "ownership verification state is
-- orthogonal and versioned." A bank account's IsOwnershipVerified fact is
-- derived by querying for any non-superseded row here, never stored as a
-- column on bank_accounts itself.
CREATE TABLE bank_account_ownership_evidence (
    evidence_id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bank_account_id            UUID NOT NULL REFERENCES bank_accounts (bank_account_id),
    tenant_id                  VARCHAR(255) NOT NULL,
    verification_method        TEXT NOT NULL,
    evidence_ref                TEXT NOT NULL,
    verified_by_principal_id  TEXT NOT NULL,
    verified_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    superseded_by               UUID REFERENCES bank_account_ownership_evidence (evidence_id)
);

CREATE INDEX idx_ownership_evidence_account ON bank_account_ownership_evidence (bank_account_id, verified_at DESC);

ALTER TABLE bank_account_ownership_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_account_ownership_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON bank_account_ownership_evidence
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- The runtime connects as Postgres superuser (bypasses GRANT/REVOKE), so
-- only BEFORE UPDATE/DELETE triggers raising exceptions actually enforce
-- immutability here — same documented caveat as every other service in
-- this codebase's Banking domain.
CREATE OR REPLACE FUNCTION reject_ownership_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'bank_account_ownership_evidence rows are never deleted';
    END IF;
    IF NEW.bank_account_id IS DISTINCT FROM OLD.bank_account_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.verification_method IS DISTINCT FROM OLD.verification_method
        OR NEW.evidence_ref IS DISTINCT FROM OLD.evidence_ref
        OR NEW.verified_by_principal_id IS DISTINCT FROM OLD.verified_by_principal_id
        OR NEW.verified_at IS DISTINCT FROM OLD.verified_at
    THEN
        RAISE EXCEPTION 'bank account ownership evidence % is append-only — only superseded_by may ever be set', OLD.evidence_id;
    END IF;
    IF OLD.superseded_by IS NOT NULL AND NEW.superseded_by IS DISTINCT FROM OLD.superseded_by THEN
        RAISE EXCEPTION 'bank account ownership evidence % is already superseded', OLD.evidence_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_ownership_evidence_mutation
    BEFORE UPDATE OR DELETE ON bank_account_ownership_evidence
    FOR EACH ROW EXECUTE FUNCTION reject_ownership_evidence_mutation();

-- CLOSED bank accounts are terminal; masked_account_number/bank_identifier
-- may only change via RotateAccountIdentifierToken, which bumps
-- token_version by exactly 1 in the same statement.
CREATE OR REPLACE FUNCTION reject_bank_account_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'bank_accounts rows are never deleted';
    END IF;
    IF OLD.account_status = 'CLOSED' THEN
        RAISE EXCEPTION 'bank account % is CLOSED and cannot be modified', OLD.bank_account_id;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'bank account % identity/ownership fields are immutable — close and recreate instead', OLD.bank_account_id;
    END IF;
    IF (NEW.masked_account_number IS DISTINCT FROM OLD.masked_account_number
        OR NEW.bank_identifier IS DISTINCT FROM OLD.bank_identifier)
        AND NEW.token_version IS DISTINCT FROM OLD.token_version + 1
    THEN
        RAISE EXCEPTION 'bank account % identifier can only change via a rotation that increments token_version by exactly 1', OLD.bank_account_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_bank_account_mutation
    BEFORE UPDATE OR DELETE ON bank_accounts
    FOR EACH ROW EXECUTE FUNCTION reject_bank_account_mutation();
