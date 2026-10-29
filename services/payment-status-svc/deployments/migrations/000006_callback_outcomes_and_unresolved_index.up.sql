-- ZS-SVC-M-001 INT-07 follow-up.
--
--   * a callback blocked by an open evidence conflict, or arriving from a provider that does not own the
--     payment, is NOT decided. Its receipt must therefore not consume the event's one-decision slot: the
--     provider keeps retrying (202) and the same event must be able to apply once the conflict is resolved
--     or the correct provider calls;
--   * ListUnresolvedPayments filters by legal entity and orders by age, so it gets a matching partial index.
-- Re-runnable.

ALTER TABLE provider_callback_receipts
    DROP CONSTRAINT IF EXISTS provider_callback_receipts_outcome_check;
ALTER TABLE provider_callback_receipts
    ADD CONSTRAINT provider_callback_receipts_outcome_check CHECK (outcome IN
        ('APPLIED', 'DUPLICATE', 'REGRESSION_BLOCKED', 'STALE_IGNORED', 'UNKNOWN_PAYMENT', 'AMBIGUOUS_PAYMENT',
         'CONFLICT_BLOCKED', 'PROVIDER_MISMATCH'));

DROP INDEX IF EXISTS uq_callback_receipt_first;
CREATE UNIQUE INDEX uq_callback_receipt_first
    ON provider_callback_receipts (tenant_id, provider_key, event_ref)
    WHERE outcome NOT IN ('DUPLICATE', 'UNKNOWN_PAYMENT', 'AMBIGUOUS_PAYMENT', 'CONFLICT_BLOCKED', 'PROVIDER_MISMATCH');

CREATE INDEX IF NOT EXISTS idx_payment_execution_states_open_conflict
    ON payment_execution_states (legal_entity_id, updated_at)
    WHERE has_open_conflict;
