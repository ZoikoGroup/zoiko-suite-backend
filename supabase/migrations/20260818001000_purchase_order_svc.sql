-- 20260818001000_purchase_order_svc.sql
-- purchase-order-svc → schema `purchase_order`
--
-- End state of 000001_initial_schema (the service's only migration).
-- Two tables: purchase_orders, purchase_order_amendments. Plus the
-- purchase_order_number_seq sequence behind po_number.

CREATE SCHEMA IF NOT EXISTS purchase_order;

COMMENT ON SCHEMA purchase_order IS
    'purchase-order-svc. PO issuance, amendment and close, with an append-only amendment ledger.';

GRANT USAGE ON SCHEMA purchase_order TO zoiko_backend, authenticated;

CREATE SEQUENCE purchase_order.purchase_order_number_seq;

-- ── purchase_orders ──────────────────────────────────────────────────────────

CREATE TABLE purchase_order.purchase_orders (
    purchase_order_id      UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID          NOT NULL,
    legal_entity_id        UUID          NOT NULL,

    purchase_request_id    UUID,
    vendor_profile_id      UUID,

    po_number              VARCHAR(32)   NOT NULL,

    -- ISSUED | AMENDED | CLOSED
    po_status              VARCHAR(20)   NOT NULL,

    total_amount           NUMERIC(18,2) NOT NULL,
    currency_code          VARCHAR(3)    NOT NULL,

    -- Bumped on each amendment; the amendment ledger records from/to.
    version                INTEGER       NOT NULL DEFAULT 1,

    issued_by_principal_id VARCHAR(255)  NOT NULL DEFAULT app.current_principal_id(),
    closed_by_principal_id VARCHAR(255),

    correlation_id         VARCHAR(255)  NOT NULL,
    created_at             TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    issued_at              TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    closed_at              TIMESTAMPTZ,

    CONSTRAINT purchase_orders_version_positive
        CHECK (version >= 1),

    -- A closed order must say who closed it and when.
    CONSTRAINT purchase_orders_closed_has_evidence
        CHECK ((po_status = 'CLOSED') = (closed_at IS NOT NULL AND closed_by_principal_id IS NOT NULL))
);

CREATE INDEX idx_purchase_orders_tenant  ON purchase_order.purchase_orders (tenant_id);
CREATE INDEX idx_purchase_orders_entity  ON purchase_order.purchase_orders (legal_entity_id);
CREATE INDEX idx_purchase_orders_status  ON purchase_order.purchase_orders (po_status);
CREATE INDEX idx_purchase_orders_request ON purchase_order.purchase_orders (purchase_request_id);

-- Idempotency: a retried Issue must return the ORIGINAL order, never mint a
-- second one.
CREATE UNIQUE INDEX idx_purchase_orders_tenant_correlation
    ON purchase_order.purchase_orders (tenant_id, correlation_id);

-- po_number is human-facing and unique per TENANT, not globally — two tenants
-- may legitimately land on the same sequence value.
CREATE UNIQUE INDEX idx_purchase_orders_tenant_po_number
    ON purchase_order.purchase_orders (tenant_id, po_number);

-- Composite key the amendment ledger's foreign key points at, so an amendment
-- cannot reference an order belonging to a different tenant. See below.
CREATE UNIQUE INDEX idx_purchase_orders_id_tenant
    ON purchase_order.purchase_orders (purchase_order_id, tenant_id);

-- ── purchase_order_amendments ────────────────────────────────────────────────
-- Append-only. One row per amendment, preserving the full before/after value
-- rather than destructively overwriting total_amount.

CREATE TABLE purchase_order.purchase_order_amendments (
    amendment_id            UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    purchase_order_id       UUID          NOT NULL,
    tenant_id               UUID          NOT NULL,

    from_version            INTEGER       NOT NULL,
    to_version              INTEGER       NOT NULL,
    previous_total_amount   NUMERIC(18,2) NOT NULL,
    new_total_amount        NUMERIC(18,2) NOT NULL,
    reason                  TEXT          NOT NULL,

    amended_by_principal_id VARCHAR(255)  NOT NULL DEFAULT app.current_principal_id(),
    amended_at              TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    -- The compose schema referenced purchase_orders(purchase_order_id) alone
    -- and carried its own tenant_id, with nothing tying the two together — so
    -- an amendment could name one tenant while its order belonged to another,
    -- and the row would be invisible to the tenant whose order actually
    -- changed. The composite reference makes that unrepresentable.
    CONSTRAINT purchase_order_amendments_order_fk
        FOREIGN KEY (purchase_order_id, tenant_id)
        REFERENCES purchase_order.purchase_orders (purchase_order_id, tenant_id),

    CONSTRAINT purchase_order_amendments_version_advances
        CHECK (to_version > from_version)
);

CREATE INDEX idx_po_amendments_order  ON purchase_order.purchase_order_amendments (purchase_order_id);
CREATE INDEX idx_po_amendments_tenant ON purchase_order.purchase_order_amendments (tenant_id);

-- Material history: mutation here would destroy the record of what the order
-- was before, so this gets the trigger as well as the withheld grant.
CREATE TRIGGER purchase_order_amendments_immutable
    BEFORE UPDATE OR DELETE ON purchase_order.purchase_order_amendments
    FOR EACH ROW EXECUTE FUNCTION app.reject_mutation();

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE purchase_order.purchase_orders            ENABLE ROW LEVEL SECURITY;
ALTER TABLE purchase_order.purchase_orders            FORCE  ROW LEVEL SECURITY;
ALTER TABLE purchase_order.purchase_order_amendments  ENABLE ROW LEVEL SECURITY;
ALTER TABLE purchase_order.purchase_order_amendments  FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON purchase_order.purchase_orders
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON purchase_order.purchase_orders
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_isolation ON purchase_order.purchase_order_amendments
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON purchase_order.purchase_order_amendments
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON purchase_order.purchase_orders           TO authenticated;
GRANT SELECT ON purchase_order.purchase_order_amendments TO authenticated;

GRANT SELECT, INSERT, UPDATE ON purchase_order.purchase_orders TO zoiko_backend;

-- Append-only: SELECT and INSERT only.
GRANT SELECT, INSERT ON purchase_order.purchase_order_amendments TO zoiko_backend;

GRANT USAGE ON SEQUENCE purchase_order.purchase_order_number_seq TO zoiko_backend;
