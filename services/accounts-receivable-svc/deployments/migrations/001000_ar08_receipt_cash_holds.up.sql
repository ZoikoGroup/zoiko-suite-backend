-- Migration: 001000_ar08_receipt_cash_holds.up.sql
-- AR-10 (refund) needs to earmark a receipt's UNAPPLIED cash so it cannot be applied to invoices (AR-08) while a refund is in
-- flight, and so it is gone for good once the refund settles. The hold is owned by AR-08 (it owns ar08_receipts); AR-10 reaches it
-- only through the ar08.Module methods ReserveUnapplied / ReleaseUnapplied / ConsumeUnapplied.
--   RESERVED  cash earmarked for an external refund            (counts against availability)
--   CONSUMED  the refund settled; the cash left the business   (counts against availability, permanently)
--   RELEASED  the refund failed / was cancelled                (cash is available again)
-- The receipt equation (amount = applied + unapplied + difference) is untouched: holds are a separate dimension and a trigger
-- on ar08_receipts refuses any change that would apply cash which is held or already refunded.

CREATE TABLE IF NOT EXISTS ar08_receipt_holds (
    hold_id         UUID PRIMARY KEY,
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    receipt_id      UUID          NOT NULL REFERENCES ar08_receipts(receipt_id),
    owner_ref       VARCHAR(255)  NOT NULL CHECK (owner_ref <> ''),
    amount          NUMERIC(20,4) NOT NULL CONSTRAINT ar08_hold_amount_positive CHECK (amount > 0),
    status          VARCHAR(10)   NOT NULL DEFAULT 'RESERVED' CONSTRAINT ar08_hold_status CHECK (status IN ('RESERVED','CONSUMED','RELEASED')),
    created_by      VARCHAR(255)  NOT NULL,
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_ar08_hold_owner UNIQUE (tenant_id, owner_ref)
);
CREATE INDEX IF NOT EXISTS idx_ar08_hold_receipt ON ar08_receipt_holds (tenant_id, receipt_id, status);

CREATE OR REPLACE FUNCTION ar08_guard_hold() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR08_HOLD_NOT_DELETABLE: a receipt hold is released or consumed, never deleted';
    END IF;
    IF NEW.hold_id <> OLD.hold_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
       OR NEW.receipt_id <> OLD.receipt_id OR NEW.owner_ref <> OLD.owner_ref OR NEW.amount <> OLD.amount OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'AR08_HOLD_IMMUTABLE: the receipt, owner and amount of a hold never change';
    END IF;
    IF OLD.status <> 'RESERVED' AND NEW.status <> OLD.status THEN
        RAISE EXCEPTION 'AR08_HOLD_FINAL: a % hold cannot change state', OLD.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar08_hold_guard ON ar08_receipt_holds;
CREATE TRIGGER ar08_hold_guard BEFORE UPDATE OR DELETE ON ar08_receipt_holds FOR EACH ROW EXECUTE FUNCTION ar08_guard_hold();

-- Applied cash + difference may never eat into held or refunded cash.
CREATE OR REPLACE FUNCTION ar08_guard_receipt_holds() RETURNS trigger AS $$
DECLARE held NUMERIC(20,4);
BEGIN
    SELECT COALESCE(sum(amount), 0) INTO held FROM ar08_receipt_holds
     WHERE tenant_id = NEW.tenant_id AND receipt_id = NEW.receipt_id AND status IN ('RESERVED','CONSUMED');
    IF held > 0 AND NEW.applied_amount + NEW.difference_amount > NEW.amount - held THEN
        RAISE EXCEPTION 'AR08_RECEIPT_CASH_RESERVED: % of this receipt is reserved for / consumed by a refund and cannot be applied', held;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar08_receipt_holds_guard ON ar08_receipts;
CREATE TRIGGER ar08_receipt_holds_guard BEFORE UPDATE ON ar08_receipts FOR EACH ROW EXECUTE FUNCTION ar08_guard_receipt_holds();

ALTER TABLE ar08_receipt_holds ENABLE ROW LEVEL SECURITY;
ALTER TABLE ar08_receipt_holds FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON ar08_receipt_holds;
CREATE POLICY tenant_isolation_policy ON ar08_receipt_holds FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
