-- BNK-06 Wave 11a: real authorization-fingerprint verification.
--
-- Before this migration, authorization_fingerprint was accepted from the
-- caller and stored, but never checked against anything — it was
-- immutable-but-unverified evidence, not an authenticated one. These two
-- new columns let PrepareAttempt independently re-fetch and compare the
-- fingerprint against its real upstream source when one is declared:
--   - authorization_id: which upstream record to re-fetch (opaque to this
--     service; interpreted by whichever service authorization_source
--     names).
--   - authorization_source: which upstream service is authoritative for
--     that ID. Only "PAYMENT_AUTHORIZATION_SVC" is verified today (Wave
--     11a, AP-10's payment-authorization-svc). An empty/unrecognized
--     source is NOT verified — this is a deliberate, temporary carve-out
--     for BNK-09-originated attempts (treasury-svc has no equivalent
--     fingerprint concept yet; see Wave 11b) so this migration doesn't
--     silently break that existing, working flow.
ALTER TABLE payment_initiation_attempts
    ADD COLUMN authorization_id     TEXT NOT NULL DEFAULT '',
    ADD COLUMN authorization_source TEXT NOT NULL DEFAULT '';

CREATE OR REPLACE FUNCTION reject_attempt_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment_initiation_attempts rows are never deleted';
    END IF;

    IF OLD.status IN ('SUBMITTED', 'REJECTED_BEFORE_SUBMISSION', 'CANCELLED', 'QUARANTINED') THEN
        RAISE EXCEPTION 'attempt % is in terminal status % and cannot be modified', OLD.attempt_id, OLD.status;
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.source_reference IS DISTINCT FROM OLD.source_reference
        OR NEW.authorization_fingerprint IS DISTINCT FROM OLD.authorization_fingerprint
        OR NEW.authorization_id IS DISTINCT FROM OLD.authorization_id
        OR NEW.authorization_source IS DISTINCT FROM OLD.authorization_source
        OR NEW.payer_account_ref IS DISTINCT FROM OLD.payer_account_ref
        OR NEW.payee_ref IS DISTINCT FROM OLD.payee_ref
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.execution_date IS DISTINCT FROM OLD.execution_date
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'attempt % is prepared; its authorized fields can never change', OLD.attempt_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
