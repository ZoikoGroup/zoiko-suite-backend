-- 000013_add_com05_payment_attempts.up.sql
-- COM-05 Platform Commercial Billing, part 5b (ZS-SVC-Q-001 §4.5; COM-CTRL-023,
-- -024, -025, -026, -027; negative paths #25, #26, #27, #28, #29).
--
-- Scope of this part: PaymentAttemptRef and the CollectionState query surface
-- — CollectPayment / RecordProviderOutcome and GetPaymentAttempts. Collection
-- is scoped to the invoice's full total_amount; partial payments are out of
-- scope for this part (not a row this doc's Charge Basis/data model tables
-- name with partial-payment semantics either).
--
-- CollectionState is deliberately NOT a second persisted state machine here:
-- it is fully derivable from an invoice's payment attempts (PAID iff a
-- SUCCEEDED attempt exists; PAYMENT_PENDING iff an unresolved one does;
-- otherwise CURRENT), so GetCollectionState (Go layer) computes it from
-- payment_attempts rather than duplicating that fact in a second table that
-- could drift from the attempts it is supposed to summarize — same
-- reasoning the BNK-05 gap-remediation plan documents for not inventing a
-- second result_status column where the state already carries the answer.
--
-- Custom SQLSTATE reused: CP001 immutable/lifecycle violation.

CREATE TABLE payment_attempts (
    attempt_id                 TEXT         PRIMARY KEY
        CHECK (attempt_id ~ '^cpat_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    organization_id              UUID         NOT NULL,
    invoice_id                    TEXT         NOT NULL REFERENCES platform_commercial_invoices (invoice_id),
    amount                         NUMERIC      NOT NULL CHECK (amount > 0),
    currency_code                   CHAR(3)      NOT NULL,
    status                           VARCHAR(16)  NOT NULL DEFAULT 'CREATED'
        CHECK (status IN ('CREATED', 'SUBMITTED', 'PENDING_UNKNOWN', 'SUCCEEDED', 'FAILED', 'VOIDED')),
    provider_attempt_ref             VARCHAR(255),
    provider_event_id                VARCHAR(255),
    settlement_ref                    VARCHAR(255),
    failure_reason                     TEXT,
    created_at                          TIMESTAMPTZ  NOT NULL,
    created_by_principal_id             VARCHAR(255) NOT NULL,
    submitted_at                         TIMESTAMPTZ,
    resolved_at                           TIMESTAMPTZ,
    resolved_by_principal_id              VARCHAR(255),
    -- COM-CTRL-027: settlement evidence before paid state.
    CONSTRAINT payment_attempts_succeeded_needs_settlement CHECK (
        status <> 'SUCCEEDED' OR settlement_ref IS NOT NULL),
    CONSTRAINT payment_attempts_failed_needs_reason CHECK (
        status <> 'FAILED' OR failure_reason IS NOT NULL),
    CONSTRAINT payment_attempts_resolved_all_or_nothing CHECK (
        (resolved_at IS NULL) = (status IN ('CREATED', 'SUBMITTED', 'PENDING_UNKNOWN')))
);

-- Negative path #27 / COM-CTRL-026 "no blind payment retry": at most one
-- unresolved attempt per invoice at a time. CollectPayment's own store logic
-- additionally returns the existing unresolved attempt instead of inserting
-- a second one (defense in depth — the index is what makes a race
-- structurally impossible even if that check were ever bypassed).
CREATE UNIQUE INDEX idx_payment_attempts_one_unresolved_per_invoice ON payment_attempts (invoice_id)
    WHERE status IN ('CREATED', 'SUBMITTED', 'PENDING_UNKNOWN');

-- Negative path #28: a duplicate processor callback (same provider_event_id)
-- is deduplicated at the database, not merely by application care.
CREATE UNIQUE INDEX idx_payment_attempts_provider_event_id ON payment_attempts (provider_event_id)
    WHERE provider_event_id IS NOT NULL;

CREATE INDEX idx_payment_attempts_invoice ON payment_attempts (invoice_id);
CREATE INDEX idx_payment_attempts_org ON payment_attempts (organization_id);

-- Negative path #29: out-of-order/regressive transitions are refused. A
-- terminal attempt (SUCCEEDED/FAILED/VOIDED) never changes again; from
-- CREATED/SUBMITTED/PENDING_UNKNOWN the only allowed next statuses are
-- SUBMITTED, PENDING_UNKNOWN, SUCCEEDED, FAILED or VOIDED — never back to
-- CREATED.
CREATE FUNCTION enforce_payment_attempt_lifecycle() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment attempt % cannot be deleted', OLD.attempt_id USING ERRCODE = 'CP001';
    END IF;
    IF OLD.status IN ('SUCCEEDED', 'FAILED', 'VOIDED') THEN
        RAISE EXCEPTION 'payment attempt % is resolved (%) and immutable', OLD.attempt_id, OLD.status USING ERRCODE = 'CP001';
    END IF;
    IF NEW.status = 'CREATED' THEN
        RAISE EXCEPTION 'payment attempt % cannot regress to CREATED', OLD.attempt_id USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_payment_attempts_lifecycle
    BEFORE UPDATE OR DELETE ON payment_attempts
    FOR EACH ROW EXECUTE FUNCTION enforce_payment_attempt_lifecycle();

-- Writes (create/submit/record outcome) are platform billing-operations
-- authority, same shape as every other COM-05 mutation; reads are the
-- organization's own or the seller plane's.
ALTER TABLE payment_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY payment_attempts_read ON payment_attempts FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY payment_attempts_seller_insert ON payment_attempts FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY payment_attempts_seller_update ON payment_attempts FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY payment_attempts_seller_delete ON payment_attempts FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');
