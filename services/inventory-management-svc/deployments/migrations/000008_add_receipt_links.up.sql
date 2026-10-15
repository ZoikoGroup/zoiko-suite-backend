-- Migration: 000008_add_receipt_links.up.sql
--
-- Ties an INV-03 RECEIPT movement to the AP-03 goods/service receipt it came
-- from, so INV-04 can post the inbound-value reclass for it.
--
-- Why: goods-service-receipt-svc (AP-03) already posts the GRNI accrual when a
-- receipt is confirmed: Dr AP_GRNI_EXPENSE / Cr AP_GRNI_ACCRUAL. For a stock
-- item that cost is therefore already in expense. INV-04 owns the monetary
-- effect of inventory (spec §8: "INV-04 determines monetary effect"), so when
-- it values the inbound movement it reclasses that expense into the
-- inventory asset: Dr Inventory / Cr AP_GRNI_EXPENSE. Only movements with a
-- recorded AP receipt link are reclassed — without one, INV cannot know the
-- cost was already expensed, and posting would double-count it.
--
-- A separate table (not a column on inventory_movements) so committed
-- movements stay untouched by the "committed movement immutable" trigger and
-- movementColumns/scanMovement are unchanged. Insert-only: a link is
-- evidence and is never edited or deleted.
CREATE TABLE inventory_movement_receipt_links (
    tenant_id                VARCHAR(255) NOT NULL,
    movement_id                UUID NOT NULL REFERENCES inventory_movements(movement_id),
    ap_receipt_id                VARCHAR(255) NOT NULL,
    linked_at                      TIMESTAMP WITH TIME ZONE NOT NULL,
    linked_by_principal_id           VARCHAR(255) NOT NULL,

    PRIMARY KEY (tenant_id, movement_id)
);

CREATE OR REPLACE FUNCTION reject_receipt_link_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'inventory_movement_receipt_links: a receipt link is immutable — % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_receipt_link_mutation
    BEFORE UPDATE OR DELETE ON inventory_movement_receipt_links
    FOR EACH ROW EXECUTE FUNCTION reject_receipt_link_mutation();

ALTER TABLE inventory_movement_receipt_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_movement_receipt_links FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_movement_receipt_links
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_receipt_links_receipt ON inventory_movement_receipt_links (tenant_id, ap_receipt_id);
