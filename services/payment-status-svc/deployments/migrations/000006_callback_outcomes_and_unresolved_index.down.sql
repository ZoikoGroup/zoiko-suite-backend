-- Reverting the receipt vocabulary requires discarding rows carrying the two
-- retryable (never-decided) outcomes: they are not finality decisions, and the
-- old CHECK cannot represent them. The append-only trigger is lifted for this
-- one destructive schema rollback.
ALTER TABLE provider_callback_receipts DISABLE TRIGGER trg_reject_callback_receipts_mutation;
DELETE FROM provider_callback_receipts WHERE outcome IN ('CONFLICT_BLOCKED', 'PROVIDER_MISMATCH');
ALTER TABLE provider_callback_receipts ENABLE TRIGGER trg_reject_callback_receipts_mutation;

ALTER TABLE provider_callback_receipts
    DROP CONSTRAINT IF EXISTS provider_callback_receipts_outcome_check;
ALTER TABLE provider_callback_receipts
    ADD CONSTRAINT provider_callback_receipts_outcome_check CHECK (outcome IN
        ('APPLIED', 'DUPLICATE', 'REGRESSION_BLOCKED', 'STALE_IGNORED', 'UNKNOWN_PAYMENT', 'AMBIGUOUS_PAYMENT'));

DROP INDEX IF EXISTS uq_callback_receipt_first;
CREATE UNIQUE INDEX uq_callback_receipt_first
    ON provider_callback_receipts (tenant_id, provider_key, event_ref)
    WHERE outcome NOT IN ('DUPLICATE', 'UNKNOWN_PAYMENT', 'AMBIGUOUS_PAYMENT');

DROP INDEX IF EXISTS idx_payment_execution_states_open_conflict;
