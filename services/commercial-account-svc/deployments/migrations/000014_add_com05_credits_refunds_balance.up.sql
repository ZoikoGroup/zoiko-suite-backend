-- 000014_add_com05_credits_refunds_balance.up.sql
-- COM-05 Platform Commercial Billing, part 5c (ZS-SVC-Q-001 §4.5; COM-CTRL-005,
-- -028; negative paths #24, #31, #33).
--
-- Scope of this part: CreditNote, RefundRequest (RequestRefund + settling its
-- outcome) and ApplyWriteOff, plus the OutstandingBalance query surface
-- (GetBalance). An issued invoice is never edited to reflect a credit,
-- refund or write-off (negative path #24: "correction through
-- credit/debit/adjustment" — §4.5 failure semantics): each is its own
-- immutable document, and OutstandingBalance is computed from the invoice
-- plus every document against it, never a second mutable running total that
-- could drift from them.
--
-- Custom SQLSTATE reused: CP001 immutable/lifecycle violation.

-- ── Credit notes (immutable correction/credit document) ───────────────────
CREATE TABLE credit_notes (
    credit_note_id            TEXT         PRIMARY KEY
        CHECK (credit_note_id ~ '^ccrn_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    organization_id             UUID         NOT NULL,
    invoice_id                    TEXT         NOT NULL REFERENCES platform_commercial_invoices (invoice_id),
    amount                          NUMERIC      NOT NULL CHECK (amount > 0),
    currency_code                    CHAR(3)      NOT NULL,
    reason                             TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    issued_at                          TIMESTAMPTZ  NOT NULL,
    issued_by_principal_id              VARCHAR(255) NOT NULL
);

CREATE INDEX idx_credit_notes_invoice ON credit_notes (invoice_id);
CREATE INDEX idx_credit_notes_org ON credit_notes (organization_id);

CREATE TRIGGER trg_credit_notes_immutable
    BEFORE UPDATE OR DELETE ON credit_notes
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

ALTER TABLE credit_notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE credit_notes FORCE ROW LEVEL SECURITY;
CREATE POLICY credit_notes_read ON credit_notes FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY credit_notes_seller_insert ON credit_notes FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY credit_notes_seller_update ON credit_notes FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY credit_notes_seller_delete ON credit_notes FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');

-- ── Write-offs (delegated authority only; negative path #33) ──────────────
CREATE TABLE write_offs (
    write_off_id              TEXT         PRIMARY KEY
        CHECK (write_off_id ~ '^cwof_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    organization_id             UUID         NOT NULL,
    invoice_id                    TEXT         NOT NULL REFERENCES platform_commercial_invoices (invoice_id),
    amount                          NUMERIC      NOT NULL CHECK (amount > 0),
    currency_code                    CHAR(3)      NOT NULL,
    reason                             TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    applied_at                         TIMESTAMPTZ  NOT NULL,
    applied_by_principal_id             VARCHAR(255) NOT NULL
);

CREATE INDEX idx_write_offs_invoice ON write_offs (invoice_id);
CREATE INDEX idx_write_offs_org ON write_offs (organization_id);

CREATE TRIGGER trg_write_offs_immutable
    BEFORE UPDATE OR DELETE ON write_offs
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

ALTER TABLE write_offs ENABLE ROW LEVEL SECURITY;
ALTER TABLE write_offs FORCE ROW LEVEL SECURITY;
CREATE POLICY write_offs_read ON write_offs FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY write_offs_seller_insert ON write_offs FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY write_offs_seller_update ON write_offs FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY write_offs_seller_delete ON write_offs FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');

-- ── Refund requests (COM-CTRL-028: refund destination fingerprint) ────────
--
-- destination_fingerprint is a SHA-256 hex digest of the destination the
-- refund was approved to pay out to, captured at request time. Settling the
-- refund requires the caller to present the same destination again; a
-- mismatch means the destination changed after approval and the refund must
-- be re-requested against the new destination rather than silently paid out
-- to it (negative path #31).
CREATE TABLE refund_requests (
    refund_id                 TEXT         PRIMARY KEY
        CHECK (refund_id ~ '^crfd_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    organization_id             UUID         NOT NULL,
    invoice_id                    TEXT         NOT NULL REFERENCES platform_commercial_invoices (invoice_id),
    payment_attempt_id              TEXT         NOT NULL REFERENCES payment_attempts (attempt_id),
    amount                            NUMERIC      NOT NULL CHECK (amount > 0),
    currency_code                      CHAR(3)      NOT NULL,
    destination_ref                     VARCHAR(255) NOT NULL CHECK (btrim(destination_ref) <> ''),
    destination_fingerprint               VARCHAR(64)  NOT NULL CHECK (destination_fingerprint ~ '^[0-9a-f]{64}$'),
    reason                                 TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    status                                  VARCHAR(16)  NOT NULL DEFAULT 'REQUESTED'
        CHECK (status IN ('REQUESTED', 'SETTLED', 'FAILED')),
    requested_at                            TIMESTAMPTZ  NOT NULL,
    requested_by_principal_id                VARCHAR(255) NOT NULL,
    settlement_ref                            VARCHAR(255),
    failure_reason                             TEXT,
    resolved_at                                 TIMESTAMPTZ,
    resolved_by_principal_id                     VARCHAR(255),
    CONSTRAINT refund_requests_settled_needs_settlement CHECK (
        status <> 'SETTLED' OR settlement_ref IS NOT NULL),
    CONSTRAINT refund_requests_failed_needs_reason CHECK (
        status <> 'FAILED' OR failure_reason IS NOT NULL),
    CONSTRAINT refund_requests_resolved_all_or_nothing CHECK (
        (resolved_at IS NULL) = (status = 'REQUESTED'))
);

CREATE INDEX idx_refund_requests_invoice ON refund_requests (invoice_id);
CREATE INDEX idx_refund_requests_attempt ON refund_requests (payment_attempt_id);
CREATE INDEX idx_refund_requests_org ON refund_requests (organization_id);

CREATE FUNCTION enforce_refund_request_lifecycle() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'refund request % cannot be deleted', OLD.refund_id USING ERRCODE = 'CP001';
    END IF;
    IF OLD.status <> 'REQUESTED' THEN
        RAISE EXCEPTION 'refund request % is resolved (%) and immutable', OLD.refund_id, OLD.status USING ERRCODE = 'CP001';
    END IF;
    IF NEW.status = 'REQUESTED' THEN
        RAISE EXCEPTION 'refund request % cannot be updated while still REQUESTED except to resolve it', OLD.refund_id
            USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_refund_requests_lifecycle
    BEFORE UPDATE OR DELETE ON refund_requests
    FOR EACH ROW EXECUTE FUNCTION enforce_refund_request_lifecycle();

ALTER TABLE refund_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE refund_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY refund_requests_read ON refund_requests FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY refund_requests_seller_insert ON refund_requests FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY refund_requests_seller_update ON refund_requests FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY refund_requests_seller_delete ON refund_requests FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');
