-- ZS-SVC-M-001 INT-07: provider callbacks, verified and evidenced.
--
--   * every AUTHENTICATED provider callback leaves exactly one receipt row saying what was done with it
--     (applied, duplicate, regression blocked, stale, unknown payment, unmapped status). Receipts are
--     append-only. Unauthenticated input (a failed signature) never writes a row: it is refused, counted and
--     logged, so a hostile caller cannot grow the table;
--   * an event from a provider is applied at most once (a unique index, not an application check);
--   * an older or lower-ranked callback can never move a payment backwards: the update itself refuses it.
-- Re-runnable.

ALTER TABLE payment_execution_states
    ADD COLUMN IF NOT EXISTS provider_key           TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_provider_event_at TIMESTAMPTZ NULL;

CREATE TABLE IF NOT EXISTS provider_callback_receipts (
    receipt_id       UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID         NOT NULL,
    provider_key     VARCHAR(80)  NOT NULL,
    event_ref        VARCHAR(128) NOT NULL,
    payment_ref      VARCHAR(128) NOT NULL,
    payment_id       UUID         NULL REFERENCES payment_execution_states (payment_id),
    legal_entity_id  UUID         NULL,
    scheme           VARCHAR(20)  NOT NULL,
    reported_status  VARCHAR(20)  NOT NULL,
    mapped_status    VARCHAR(20)  NOT NULL DEFAULT '',
    event_time       TIMESTAMPTZ  NULL,
    body_hash        VARCHAR(64)  NOT NULL CHECK (body_hash ~ '^[0-9a-f]{64}$'),
    outcome          VARCHAR(24)  NOT NULL CHECK (outcome IN
        ('APPLIED', 'DUPLICATE', 'REGRESSION_BLOCKED', 'STALE_IGNORED', 'UNKNOWN_PAYMENT', 'AMBIGUOUS_PAYMENT')),
    detail           TEXT         NOT NULL DEFAULT '',
    received_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- An event identity is decided once; later copies are recorded as DUPLICATE and change nothing. An event
-- that found no (or no unique) payment is NOT decided: the provider may call before the payment is known,
-- and the same event must then be able to apply.
CREATE UNIQUE INDEX IF NOT EXISTS uq_callback_receipt_first
    ON provider_callback_receipts (tenant_id, provider_key, event_ref) WHERE outcome NOT IN ('DUPLICATE', 'UNKNOWN_PAYMENT', 'AMBIGUOUS_PAYMENT');
CREATE INDEX IF NOT EXISTS idx_callback_receipts_payment ON provider_callback_receipts (payment_id, received_at);
CREATE INDEX IF NOT EXISTS idx_callback_receipts_entity ON provider_callback_receipts (legal_entity_id, received_at DESC);

DROP TRIGGER IF EXISTS trg_reject_callback_receipts_mutation ON provider_callback_receipts;
CREATE TRIGGER trg_reject_callback_receipts_mutation
    BEFORE UPDATE OR DELETE ON provider_callback_receipts
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

ALTER TABLE provider_callback_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_callback_receipts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON provider_callback_receipts;
CREATE POLICY tenant_isolation ON provider_callback_receipts FOR ALL
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
