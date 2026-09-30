-- 0040_canonical_subledgers_open_items.sql
-- ZoikoSuite Canonical AR/AP Subledgers, Open Items & Cash Applications
-- (ZS-DATA-001 §12, Invariants D06, D08)
--
-- DOCTRINE:
-- 1. Subledger Open-Item Integrity: Open receivable/payable items track residual balances.
--    Allocations and settlements cannot exceed open amounts (no over-settlement).
-- 2. Party Role Attachment: Counterparties attach via customer_party_role_id / supplier_party_role_id
--    pointing to party_master.party_roles, maintaining the single-identity party model.
-- 3. Exact Precision: Amounts use NUMERIC(38,12) exact arithmetic.

CREATE SCHEMA IF NOT EXISTS accounts_receivable;
CREATE SCHEMA IF NOT EXISTS accounts_payable;

GRANT USAGE ON SCHEMA accounts_receivable TO zoiko_backend, authenticated;
GRANT USAGE ON SCHEMA accounts_payable TO zoiko_backend, authenticated;

-- ── receivable_open_items (AR-OPEN) ───────────────────────────────────────────

CREATE TABLE accounts_receivable.receivable_open_items (
    receivable_open_item_id             UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    legal_entity_id                     UUID          NOT NULL,
    source_document_id                  UUID          NOT NULL, -- sales_invoice_id / debit_note_id
    customer_party_role_id              UUID          NOT NULL,
    original_amount                     NUMERIC(38,12) NOT NULL,
    open_amount                         NUMERIC(38,12) NOT NULL,
    currency_code                       VARCHAR(3)    NOT NULL,
    due_date                            DATE          NOT NULL,
    status                              VARCHAR(20)   NOT NULL DEFAULT 'OPEN', -- 'OPEN' | 'PART_SETTLED' | 'SETTLED' | 'DISPUTED' | 'WRITTEN_OFF'
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_roi_status CHECK (status IN ('OPEN', 'PART_SETTLED', 'SETTLED', 'DISPUTED', 'WRITTEN_OFF')),
    CONSTRAINT chk_roi_amounts CHECK (original_amount > 0 AND open_amount >= 0 AND open_amount <= original_amount),
    CONSTRAINT fk_roi_party_role FOREIGN KEY (customer_party_role_id) REFERENCES party_master.party_roles(party_role_id) ON DELETE RESTRICT
);

CREATE INDEX idx_roi_tenant_customer ON accounts_receivable.receivable_open_items (tenant_id, customer_party_role_id, status);
CREATE INDEX idx_roi_tenant_due ON accounts_receivable.receivable_open_items (tenant_id, due_date);

ALTER TABLE accounts_receivable.receivable_open_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts_receivable.receivable_open_items FORCE ROW LEVEL SECURITY;

CREATE POLICY roi_tenant_isolation ON accounts_receivable.receivable_open_items
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON accounts_receivable.receivable_open_items TO zoiko_backend, authenticated;

-- ── cash_receipts (AR-RECEIPT) ────────────────────────────────────────────────

CREATE TABLE accounts_receivable.cash_receipts (
    cash_receipt_id                     UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    legal_entity_id                     UUID          NOT NULL,
    bank_transaction_id                 UUID,
    payer_party_id                      UUID,
    amount                              NUMERIC(38,12) NOT NULL,
    unapplied_amount                    NUMERIC(38,12) NOT NULL,
    currency_code                       VARCHAR(3)    NOT NULL,
    received_at                         TIMESTAMPTZ   NOT NULL,
    reference                           TEXT,
    status                              VARCHAR(20)   NOT NULL DEFAULT 'UNAPPLIED', -- 'UNAPPLIED' | 'PART_APPLIED' | 'APPLIED' | 'RETURNED'
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_receipt_status CHECK (status IN ('UNAPPLIED', 'PART_APPLIED', 'APPLIED', 'RETURNED')),
    CONSTRAINT chk_receipt_amounts CHECK (amount > 0 AND unapplied_amount >= 0 AND unapplied_amount <= amount),
    CONSTRAINT fk_receipt_payer FOREIGN KEY (payer_party_id) REFERENCES party_master.parties(party_id) ON DELETE RESTRICT
);

CREATE INDEX idx_cash_receipts_tenant ON accounts_receivable.cash_receipts (tenant_id, legal_entity_id, status);

ALTER TABLE accounts_receivable.cash_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts_receivable.cash_receipts FORCE ROW LEVEL SECURITY;

CREATE POLICY cash_receipts_tenant_isolation ON accounts_receivable.cash_receipts
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON accounts_receivable.cash_receipts TO zoiko_backend, authenticated;

-- ── cash_applications (AR-APPLY) ──────────────────────────────────────────────

CREATE TABLE accounts_receivable.cash_applications (
    cash_application_id                 UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    cash_receipt_id                     UUID          NOT NULL,
    receivable_open_item_id             UUID          NOT NULL,
    applied_amount                      NUMERIC(38,12) NOT NULL,
    application_date                    DATE          NOT NULL,
    fx_difference_amount                NUMERIC(38,12) NOT NULL DEFAULT 0.000000000000,
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_app_amount_positive CHECK (applied_amount > 0),
    CONSTRAINT fk_app_receipt FOREIGN KEY (cash_receipt_id) REFERENCES accounts_receivable.cash_receipts(cash_receipt_id) ON DELETE RESTRICT,
    CONSTRAINT fk_app_open_item FOREIGN KEY (receivable_open_item_id) REFERENCES accounts_receivable.receivable_open_items(receivable_open_item_id) ON DELETE RESTRICT
);

CREATE INDEX idx_cash_applications_tenant ON accounts_receivable.cash_applications (tenant_id, cash_receipt_id, receivable_open_item_id);

ALTER TABLE accounts_receivable.cash_applications ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts_receivable.cash_applications FORCE ROW LEVEL SECURITY;

CREATE POLICY cash_applications_tenant_isolation ON accounts_receivable.cash_applications
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON accounts_receivable.cash_applications TO zoiko_backend, authenticated;

-- ── payable_open_items (AP-OPEN) ──────────────────────────────────────────────

CREATE TABLE accounts_payable.payable_open_items (
    payable_open_item_id                UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    legal_entity_id                     UUID          NOT NULL,
    source_document_id                  UUID          NOT NULL, -- supplier_invoice_id / credit_note_id
    supplier_party_role_id              UUID          NOT NULL,
    original_amount                     NUMERIC(38,12) NOT NULL,
    open_amount                         NUMERIC(38,12) NOT NULL,
    currency_code                       VARCHAR(3)    NOT NULL,
    due_date                            DATE          NOT NULL,
    status                              VARCHAR(20)   NOT NULL DEFAULT 'OPEN', -- 'OPEN' | 'PART_SETTLED' | 'SETTLED' | 'DISPUTED'
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_poi_status CHECK (status IN ('OPEN', 'PART_SETTLED', 'SETTLED', 'DISPUTED')),
    CONSTRAINT chk_poi_amounts CHECK (original_amount > 0 AND open_amount >= 0 AND open_amount <= original_amount),
    CONSTRAINT fk_poi_party_role FOREIGN KEY (supplier_party_role_id) REFERENCES party_master.party_roles(party_role_id) ON DELETE RESTRICT
);

CREATE INDEX idx_poi_tenant_supplier ON accounts_payable.payable_open_items (tenant_id, supplier_party_role_id, status);
CREATE INDEX idx_poi_tenant_due ON accounts_payable.payable_open_items (tenant_id, due_date);

ALTER TABLE accounts_payable.payable_open_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts_payable.payable_open_items FORCE ROW LEVEL SECURITY;

CREATE POLICY poi_tenant_isolation ON accounts_payable.payable_open_items
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON accounts_payable.payable_open_items TO zoiko_backend, authenticated;
