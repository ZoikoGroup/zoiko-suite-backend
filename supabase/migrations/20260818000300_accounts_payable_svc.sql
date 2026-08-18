-- 20260818000300_accounts_payable_svc.sql
-- accounts-payable-svc → schema `accounts_payable`
--
-- Squashed end state of 000001_initial_schema, 000002_add_idempotency_index
-- and 000003_add_source_contract_id. One table: vendor_invoices.
--
-- ── A note the compose migration's own header makes, now obsolete ────────────
-- 000001 says RLS here is "defense-in-depth, not the sole isolation guarantee"
-- because the services connect as a Postgres superuser which bypasses RLS
-- unconditionally — a fact found through a real CI failure, not theory. Under
-- `zoiko_backend` that is no longer true: the policy below is load-bearing. The
-- store layer's explicit `tenant_id = $1` predicates stay as belt and braces.
--
-- ── Tenant comparison is done as TEXT ────────────────────────────────────────
-- tenant_id is UUID here, and the compose policy cast the setting with
-- `current_setting('app.tenant_id')::UUID`. Two problems carried by that form:
-- it omits the missing_ok flag, so an unset tenant RAISES rather than returning
-- NULL; and a non-UUID value raises on the cast. Comparing
-- `tenant_id::text = app.current_tenant_id()` degrades to "matches nothing"
-- instead of erroring, which is the fail-closed behaviour wanted.

CREATE SCHEMA IF NOT EXISTS accounts_payable;

COMMENT ON SCHEMA accounts_payable IS
    'accounts-payable-svc. Vendor invoice headers through the create → validate → approve → request-payment lifecycle.';

GRANT USAGE ON SCHEMA accounts_payable TO zoiko_backend, authenticated;

-- ── vendor_invoices ──────────────────────────────────────────────────────────
-- No vendors table: no Vendor Master service exists anywhere on the platform,
-- so vendor_id is a plain, unvalidated string column.

CREATE TABLE accounts_payable.vendor_invoices (
    invoice_id                        UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                         UUID          NOT NULL,
    legal_entity_id                   UUID          NOT NULL,

    vendor_id                         VARCHAR(255)  NOT NULL,
    invoice_number                    VARCHAR(255)  NOT NULL,
    amount                            NUMERIC(18,2) NOT NULL,
    currency_code                     VARCHAR(3)    NOT NULL,
    due_date                          DATE          NOT NULL,

    -- RECEIVED | VALIDATED | APPROVED | PAYMENT_REQUESTED
    status                            VARCHAR(20)   NOT NULL,

    -- Which contract (if any) this invoice was issued against. Nullable
    -- because not every vendor invoice is tied to one; nothing fabricates a
    -- contract_id when there is no real contract behind an invoice.
    source_contract_id                UUID,

    -- Attribution defaults to the verified principal rather than the request
    -- body. Fail-closed: NULL principal + NOT NULL column rejects the write.
    created_by_principal_id           VARCHAR(255)  NOT NULL DEFAULT app.current_principal_id(),
    validated_by_principal_id         VARCHAR(255),
    approved_by_principal_id          VARCHAR(255),
    payment_requested_by_principal_id VARCHAR(255),

    correlation_id                    VARCHAR(255)  NOT NULL,
    created_at                        TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    validated_at                      TIMESTAMPTZ,
    approved_at                       TIMESTAMPTZ,
    payment_requested_at              TIMESTAMPTZ,

    UNIQUE (tenant_id, vendor_id, invoice_number)
);

CREATE INDEX idx_vendor_invoices_tenant        ON accounts_payable.vendor_invoices (tenant_id);
CREATE INDEX idx_vendor_invoices_entity_vendor ON accounts_payable.vendor_invoices (legal_entity_id, vendor_id);
CREATE INDEX idx_vendor_invoices_status        ON accounts_payable.vendor_invoices (status);

-- Idempotency: a retried CreateInvoice — after a client-side timeout on a POST
-- that actually succeeded server-side — must resolve to the ORIGINAL invoice
-- rather than create a duplicate liability.
--
-- Partial (WHERE correlation_id != '') is kept from the compose migration. On a
-- fresh database there are no legacy blank rows, but the service still permits
-- an empty correlation_id, and several such rows must not collide as false
-- duplicates.
CREATE UNIQUE INDEX idx_vendor_invoices_tenant_correlation
    ON accounts_payable.vendor_invoices (tenant_id, correlation_id)
    WHERE correlation_id != '';

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE accounts_payable.vendor_invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts_payable.vendor_invoices FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON accounts_payable.vendor_invoices
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON accounts_payable.vendor_invoices
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON accounts_payable.vendor_invoices TO authenticated;

-- No DELETE: an invoice is a liability record and transitions through statuses;
-- it is never removed.
GRANT SELECT, INSERT, UPDATE ON accounts_payable.vendor_invoices TO zoiko_backend;
