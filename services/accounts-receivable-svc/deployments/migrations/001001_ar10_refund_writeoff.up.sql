-- Migration: 001001_ar10_refund_writeoff.up.sql
-- AR-10 Customer Refund / Write-Off (spec ZS-SVC-C-001 section 13). Invariants 5, 12, 13, 14; section 18 #29..#35.
--   ar10_refunds     one logical refund per source reservation (unique owner on the AR-08 hold); the external attempt id is durable
--   ar10_writeoffs   write-off request with controlled reason, delegated authority evidence and tax treatment
--   ar10_history     append-only lifecycle evidence of both aggregates

CREATE TABLE IF NOT EXISTS ar10_refunds (
    refund_id               UUID PRIMARY KEY,
    tenant_id               UUID          NOT NULL,
    legal_entity_id         UUID          NOT NULL,
    customer_party_id       UUID,
    receipt_id              UUID          NOT NULL,
    related_open_item_id    UUID,
    currency                CHAR(3)       NOT NULL,
    amount                  NUMERIC(20,4) NOT NULL CONSTRAINT ar10_refund_amount_positive CHECK (amount > 0),
    destination_ref         VARCHAR(255)  NOT NULL CHECK (destination_ref <> ''),
    destination_fingerprint VARCHAR(128)  NOT NULL CHECK (destination_fingerprint <> ''),
    destination_verification JSONB        NOT NULL,
    payer_account_ref       VARCHAR(255)  NOT NULL CHECK (payer_account_ref <> ''),
    reason_code             VARCHAR(64)   NOT NULL CHECK (reason_code <> ''),
    evidence_id             VARCHAR(255)  NOT NULL CHECK (evidence_id <> ''),
    state                   VARCHAR(20)   NOT NULL DEFAULT 'PENDING_APPROVAL'
        CONSTRAINT ar10_refund_state CHECK (state IN ('DRAFT','PENDING_APPROVAL','APPROVED','SUBMITTED','PENDING','UNKNOWN','SETTLED','FAILED','CANCELLED')),
    binding_hash            VARCHAR(64)   NOT NULL,
    approved_hash           VARCHAR(64),
    requested_by            VARCHAR(255)  NOT NULL,
    approved_by             VARCHAR(255),
    approved_at             TIMESTAMPTZ,
    submitted_by            VARCHAR(255),
    submitted_at            TIMESTAMPTZ,
    attempt_idempotency_key VARCHAR(255),
    external_attempt_id     VARCHAR(128),
    provider_request_id     VARCHAR(255),
    payment_status_id       VARCHAR(128),
    external_state          VARCHAR(40)   NOT NULL DEFAULT '',
    payment_state           VARCHAR(40)   NOT NULL DEFAULT '',
    failure_reason          VARCHAR(1000),
    settled_at              TIMESTAMPTZ,
    accounting_state        VARCHAR(12)   CHECK (accounting_state IN ('PENDING','POSTED','FAILED','QUARANTINED')),
    journal_id              VARCHAR(128),
    accounting_error        VARCHAR(1000),
    control_exception       VARCHAR(500),
    cancel_reason           VARCHAR(255),
    version                 INTEGER       NOT NULL DEFAULT 1,
    created_at              TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT ar10_refund_sod CHECK (approved_by IS NULL OR approved_by <> requested_by),
    CONSTRAINT ar10_refund_approval_bound CHECK ((approved_by IS NULL) = (approved_hash IS NULL)),
    CONSTRAINT ar10_refund_release_sod CHECK (submitted_by IS NULL OR submitted_by <> requested_by)
);
-- One external attempt id and one payment-tracking id belong to exactly one refund (no duplicate external effect).
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar10_refund_attempt ON ar10_refunds (tenant_id, external_attempt_id) WHERE external_attempt_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar10_refund_payment ON ar10_refunds (tenant_id, payment_status_id) WHERE payment_status_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ar10_refund_receipt ON ar10_refunds (tenant_id, receipt_id);
CREATE INDEX IF NOT EXISTS idx_ar10_refund_scope ON ar10_refunds (tenant_id, legal_entity_id, state);

CREATE OR REPLACE FUNCTION ar10_guard_refund() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR10_REFUND_NOT_DELETABLE: a refund is cancelled or failed, never deleted';
    END IF;
    IF NEW.refund_id <> OLD.refund_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
       OR NEW.receipt_id <> OLD.receipt_id OR NEW.currency <> OLD.currency OR NEW.amount <> OLD.amount
       OR NEW.requested_by <> OLD.requested_by OR NEW.reason_code <> OLD.reason_code
       OR NEW.customer_party_id IS DISTINCT FROM OLD.customer_party_id THEN
        RAISE EXCEPTION 'AR10_REFUND_IMMUTABLE: the source, amount, currency and requester of a refund never change';
    END IF;
    IF OLD.state IN ('SETTLED','FAILED','CANCELLED') AND NEW.state <> OLD.state THEN
        RAISE EXCEPTION 'AR10_REFUND_FINAL: a % refund cannot change state', OLD.state;
    END IF;
    IF OLD.state IN ('SETTLED','FAILED','CANCELLED') AND
       (NEW.destination_ref <> OLD.destination_ref OR NEW.payer_account_ref <> OLD.payer_account_ref OR NEW.external_attempt_id IS DISTINCT FROM OLD.external_attempt_id) THEN
        RAISE EXCEPTION 'AR10_REFUND_FINAL: a % refund is frozen', OLD.state;
    END IF;
    -- Once the instruction left AR (SUBMITTED onward) the destination, payer account and approval are frozen.
    IF OLD.state IN ('SUBMITTED','PENDING','UNKNOWN') AND
       (NEW.destination_ref <> OLD.destination_ref OR NEW.destination_fingerprint <> OLD.destination_fingerprint
        OR NEW.payer_account_ref <> OLD.payer_account_ref OR NEW.approved_hash IS DISTINCT FROM OLD.approved_hash) THEN
        RAISE EXCEPTION 'AR10_REFUND_SUBMITTED: the destination of a submitted refund cannot change';
    END IF;
    -- Approved: the bound content cannot change in place; a change goes back to PENDING_APPROVAL with the approval cleared.
    IF OLD.state = 'APPROVED' AND NEW.state = 'APPROVED' AND
       (NEW.destination_ref <> OLD.destination_ref OR NEW.destination_fingerprint <> OLD.destination_fingerprint
        OR NEW.payer_account_ref <> OLD.payer_account_ref OR NEW.binding_hash <> OLD.binding_hash) THEN
        RAISE EXCEPTION 'AR10_REFUND_APPROVED_BOUND: an approved refund cannot be edited; changing it invalidates the approval';
    END IF;
    -- A durable external attempt id is never replaced or removed.
    IF OLD.external_attempt_id IS NOT NULL AND NEW.external_attempt_id IS DISTINCT FROM OLD.external_attempt_id THEN
        RAISE EXCEPTION 'AR10_REFUND_ATTEMPT_ID_IMMUTABLE: the external attempt id of a refund never changes';
    END IF;
    IF OLD.payment_status_id IS NOT NULL AND NEW.payment_status_id IS DISTINCT FROM OLD.payment_status_id THEN
        RAISE EXCEPTION 'AR10_REFUND_PAYMENT_ID_IMMUTABLE: the payment tracking id of a refund never changes';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar10_refund_guard ON ar10_refunds;
CREATE TRIGGER ar10_refund_guard BEFORE UPDATE OR DELETE ON ar10_refunds FOR EACH ROW EXECUTE FUNCTION ar10_guard_refund();

CREATE TABLE IF NOT EXISTS ar10_writeoffs (
    writeoff_id           UUID PRIMARY KEY,
    tenant_id             UUID          NOT NULL,
    legal_entity_id       UUID          NOT NULL,
    open_item_id          UUID          NOT NULL,
    customer_party_id     UUID          NOT NULL,
    currency              CHAR(3)       NOT NULL,
    amount                NUMERIC(20,4) NOT NULL CONSTRAINT ar10_writeoff_amount_positive CHECK (amount > 0),
    residual_at_request   NUMERIC(20,4) NOT NULL,
    reason_category       VARCHAR(32)   NOT NULL CHECK (reason_category IN ('BAD_DEBT','INSOLVENCY','UNCOLLECTIBLE_SMALL_BALANCE','STATUTE_BARRED','SETTLEMENT_DISCOUNT')),
    reason_code           VARCHAR(64)   NOT NULL CHECK (reason_code <> ''),
    evidence_id           VARCHAR(255)  NOT NULL CHECK (evidence_id <> ''),
    tax_treatment         VARCHAR(20)   NOT NULL CHECK (tax_treatment IN ('NONE_REQUIRED','RELIEF_CLAIMED','REVIEW_REQUIRED')),
    tax_relief_amount     NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (tax_relief_amount >= 0),
    tax_reviewer          VARCHAR(255),
    tax_evidence_id       VARCHAR(255),
    tax_reviewed_at       TIMESTAMPTZ,
    required_tier         INTEGER       NOT NULL CHECK (required_tier BETWEEN 1 AND 4),
    requester_tier        INTEGER       NOT NULL DEFAULT 0,
    state                 VARCHAR(20)   NOT NULL DEFAULT 'PENDING_APPROVAL'
        CONSTRAINT ar10_writeoff_state CHECK (state IN ('DRAFT','PENDING_APPROVAL','APPROVED','COMMITTED')),
    binding_hash          VARCHAR(64)   NOT NULL,
    approved_hash         VARCHAR(64),
    requested_by          VARCHAR(255)  NOT NULL,
    approved_by           VARCHAR(255),
    approved_tier         INTEGER,
    approved_at           TIMESTAMPTZ,
    committed_by          VARCHAR(255),
    committed_at          TIMESTAMPTZ,
    fiscal_period         VARCHAR(16),
    accounting_state      VARCHAR(12)   CHECK (accounting_state IN ('PENDING','POSTED','FAILED','QUARANTINED')),
    journal_id            VARCHAR(128),
    accounting_error      VARCHAR(1000),
    version               INTEGER       NOT NULL DEFAULT 1,
    created_at            TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT ar10_writeoff_sod CHECK (approved_by IS NULL OR approved_by <> requested_by),
    CONSTRAINT ar10_writeoff_approval_bound CHECK ((approved_by IS NULL) = (approved_hash IS NULL)),
    CONSTRAINT ar10_writeoff_relief_bound CHECK (tax_relief_amount <= amount AND (tax_treatment = 'RELIEF_CLAIMED' OR tax_relief_amount = 0)),
    CONSTRAINT ar10_writeoff_tax_cleared CHECK (state <> 'COMMITTED' OR tax_treatment <> 'REVIEW_REQUIRED')
);
-- One in-flight write-off per open item (two concurrent requests cannot both proceed).
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar10_writeoff_inflight ON ar10_writeoffs (tenant_id, open_item_id) WHERE state IN ('DRAFT','PENDING_APPROVAL','APPROVED');
CREATE INDEX IF NOT EXISTS idx_ar10_writeoff_item ON ar10_writeoffs (tenant_id, open_item_id);
CREATE INDEX IF NOT EXISTS idx_ar10_writeoff_scope ON ar10_writeoffs (tenant_id, legal_entity_id, state);

CREATE OR REPLACE FUNCTION ar10_guard_writeoff() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR10_WRITEOFF_NOT_DELETABLE: a write-off is never deleted';
    END IF;
    IF NEW.writeoff_id <> OLD.writeoff_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
       OR NEW.open_item_id <> OLD.open_item_id OR NEW.currency <> OLD.currency OR NEW.amount <> OLD.amount
       OR NEW.requested_by <> OLD.requested_by OR NEW.reason_category <> OLD.reason_category OR NEW.reason_code <> OLD.reason_code
       OR NEW.customer_party_id <> OLD.customer_party_id THEN
        RAISE EXCEPTION 'AR10_WRITEOFF_IMMUTABLE: the item, amount, reason and requester of a write-off never change';
    END IF;
    IF OLD.state = 'COMMITTED' AND (NEW.state <> 'COMMITTED' OR NEW.tax_treatment <> OLD.tax_treatment OR NEW.tax_relief_amount <> OLD.tax_relief_amount
       OR NEW.approved_hash IS DISTINCT FROM OLD.approved_hash OR NEW.approved_by IS DISTINCT FROM OLD.approved_by OR NEW.committed_by IS DISTINCT FROM OLD.committed_by) THEN
        RAISE EXCEPTION 'AR10_WRITEOFF_COMMITTED: a committed write-off is frozen; only its accounting mirror moves';
    END IF;
    IF OLD.state = 'APPROVED' AND NEW.state = 'APPROVED' AND
       (NEW.tax_treatment <> OLD.tax_treatment OR NEW.tax_relief_amount <> OLD.tax_relief_amount OR NEW.binding_hash <> OLD.binding_hash) THEN
        RAISE EXCEPTION 'AR10_WRITEOFF_APPROVED_BOUND: an approved write-off cannot be edited; changing its tax treatment invalidates the approval';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar10_writeoff_guard ON ar10_writeoffs;
CREATE TRIGGER ar10_writeoff_guard BEFORE UPDATE OR DELETE ON ar10_writeoffs FOR EACH ROW EXECUTE FUNCTION ar10_guard_writeoff();

CREATE TABLE IF NOT EXISTS ar10_history (
    history_id      UUID PRIMARY KEY,
    seq             BIGSERIAL     NOT NULL,
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    aggregate_type  VARCHAR(16)   NOT NULL CHECK (aggregate_type IN ('REFUND','WRITEOFF')),
    aggregate_id    UUID          NOT NULL,
    command         VARCHAR(64)   NOT NULL,
    from_state      VARCHAR(20)   NOT NULL DEFAULT '',
    to_state        VARCHAR(20)   NOT NULL,
    actor_id        VARCHAR(255)  NOT NULL,
    detail          JSONB,
    recorded_at     TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ar10_history_agg ON ar10_history (tenant_id, aggregate_id, seq);
DROP TRIGGER IF EXISTS ar10_history_append_only ON ar10_history;
CREATE TRIGGER ar10_history_append_only BEFORE UPDATE OR DELETE ON ar10_history FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ar10_refunds','ar10_writeoffs','ar10_history'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;
