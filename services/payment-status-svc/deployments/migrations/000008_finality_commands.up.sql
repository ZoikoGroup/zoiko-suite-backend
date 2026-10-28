-- ZS-SVC-M-001 BNK-07 A1: first-class ConfirmSettlement / RecordRejection.
--
-- BNK-07 §7 names both commands and requires "confirmation/return/reject
-- evidence", provider/bank references, effective timestamps and a finality
-- basis. The service previously had no durable place for manual finality
-- evidence and no command-idempotency record for a manual finality command.
--
-- payment_finality_evidence is append-only: one row per command per payment.
-- It is written in the same transaction as the status change, its
-- status_events row and its PaymentSettled/PaymentRejected outbox event.
--
-- payment_command_idempotency claims an Idempotency-Key in that same
-- transaction. The key is unique per tenant, names the command and payment it
-- acted on, and stores a hash of the exact request so the same key with a
-- different request is a conflict rather than a replay. A refused command
-- never consumes its key because the whole transaction rolls back.
--
-- The down migration refuses to run while any evidence, claim or manual-finality
-- state depends on the mechanism.

CREATE TABLE IF NOT EXISTS payment_finality_evidence (
    evidence_id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID         NULL,
    payment_id           UUID         NOT NULL REFERENCES payment_execution_states (payment_id),
    command              TEXT         NOT NULL CHECK (command IN ('CONFIRM_SETTLEMENT', 'RECORD_REJECTION')),
    provider_reference   TEXT         NOT NULL DEFAULT '',
    bank_reference       TEXT         NOT NULL DEFAULT '',
    reason               TEXT         NOT NULL,
    effective_at         TIMESTAMPTZ  NOT NULL,
    evidence_reference   TEXT         NOT NULL,
    evidence_hash        TEXT         NOT NULL DEFAULT '',
    mapping_version      TEXT         NOT NULL,
    finality_source      TEXT         NOT NULL CHECK (finality_source IN ('MANUAL_CONFIRMATION', 'MANUAL_REJECTION')),
    actor_principal_id   TEXT         NOT NULL,
    idempotency_key      TEXT         NOT NULL,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT payment_finality_evidence_has_reference CHECK (provider_reference <> '' OR bank_reference <> ''),
    CONSTRAINT payment_finality_evidence_hash_format CHECK (evidence_hash = '' OR evidence_hash ~ '^[0-9a-f]{64}$')
);

-- One finality decision of each kind per payment. A retry is resolved by the
-- idempotency claim, not by writing a second evidence row.
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_finality_evidence_payment_command
    ON payment_finality_evidence (payment_id, command);
CREATE INDEX IF NOT EXISTS idx_payment_finality_evidence_payment
    ON payment_finality_evidence (payment_id, created_at ASC);

CREATE TABLE IF NOT EXISTS payment_command_idempotency (
    tenant_id       UUID         NULL,
    idempotency_key TEXT         NOT NULL CHECK (idempotency_key <> ''),
    command         TEXT         NOT NULL CHECK (command IN ('CONFIRM_SETTLEMENT', 'RECORD_REJECTION')),
    payment_id      UUID         NOT NULL REFERENCES payment_execution_states (payment_id),
    request_hash    TEXT         NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- tenant_id is nullable for legacy/tenant-less rows; COALESCE gives those rows
-- one empty tenant key, matching the application's lookup predicate.
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_command_idempotency_tenant_key
    ON payment_command_idempotency (COALESCE(tenant_id::text, ''), idempotency_key);
CREATE INDEX IF NOT EXISTS idx_payment_command_idempotency_payment
    ON payment_command_idempotency (payment_id);

DROP TRIGGER IF EXISTS trg_reject_payment_finality_evidence_mutation ON payment_finality_evidence;
CREATE TRIGGER trg_reject_payment_finality_evidence_mutation
    BEFORE UPDATE OR DELETE ON payment_finality_evidence
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

DROP TRIGGER IF EXISTS trg_reject_payment_command_idempotency_mutation ON payment_command_idempotency;
CREATE TRIGGER trg_reject_payment_command_idempotency_mutation
    BEFORE UPDATE OR DELETE ON payment_command_idempotency
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

ALTER TABLE payment_finality_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_finality_evidence FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON payment_finality_evidence;
CREATE POLICY tenant_isolation ON payment_finality_evidence FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE payment_command_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_command_idempotency FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON payment_command_idempotency;
CREATE POLICY tenant_isolation ON payment_command_idempotency FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
