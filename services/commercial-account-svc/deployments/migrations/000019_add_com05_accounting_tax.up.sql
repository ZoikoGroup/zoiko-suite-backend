-- 000019_add_com05_accounting_tax.up.sql
-- COM-05 gap-remediation: D2 (tax jurisdiction/rate must be a real,
-- seller-registered fact, not a bare caller assertion) and D3 (a seller
-- accounting-event mapping key, server-resolved on billing_accounts exactly
-- like selling_entity, so COM-CTRL-033's isolation control has something
-- real to test against).

-- ── D3: accounting mapping key ──────────────────────────────────────────────
ALTER TABLE billing_accounts ADD COLUMN accounting_mapping_key VARCHAR(128);
UPDATE billing_accounts SET accounting_mapping_key = 'legacy-' || billing_account_id WHERE accounting_mapping_key IS NULL;
ALTER TABLE billing_accounts ALTER COLUMN accounting_mapping_key SET NOT NULL;
ALTER TABLE billing_accounts ADD CONSTRAINT billing_accounts_accounting_mapping_key_not_blank CHECK (btrim(accounting_mapping_key) <> '');

-- ── D2: registered tax jurisdictions ────────────────────────────────────────
--
-- Seller-managed reference data, mutable-in-place (upsert), the same
-- doctrine already used for commercial_currencies and meter_definitions:
-- a caller's asserted tax jurisdiction/rate must match a fact the seller
-- has actually registered, not be taken on faith.
CREATE TABLE billing_tax_jurisdictions (
    billing_account_id           TEXT         NOT NULL REFERENCES billing_accounts (billing_account_id),
    jurisdiction_code             VARCHAR(64)  NOT NULL CHECK (btrim(jurisdiction_code) <> ''),
    organization_id                 UUID         NOT NULL,
    registered_rate_basis_points      INT,
    effective_from                     TIMESTAMPTZ  NOT NULL,
    created_at                          TIMESTAMPTZ  NOT NULL,
    created_by_principal_id              VARCHAR(255) NOT NULL,
    PRIMARY KEY (billing_account_id, jurisdiction_code),
    CONSTRAINT billing_tax_jurisdictions_rate_range CHECK (
        registered_rate_basis_points IS NULL
        OR (registered_rate_basis_points >= 0 AND registered_rate_basis_points <= 10000))
);

CREATE INDEX idx_billing_tax_jurisdictions_org ON billing_tax_jurisdictions (organization_id);

ALTER TABLE billing_tax_jurisdictions ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing_tax_jurisdictions FORCE ROW LEVEL SECURITY;
CREATE POLICY billing_tax_jurisdictions_read ON billing_tax_jurisdictions FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY billing_tax_jurisdictions_seller_insert ON billing_tax_jurisdictions FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY billing_tax_jurisdictions_seller_update ON billing_tax_jurisdictions FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
