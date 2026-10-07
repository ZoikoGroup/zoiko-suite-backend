-- 0041_canonical_tax_engine_and_rules.sql
-- ZoikoSuite Canonical Tax Engine, Versioned Rule Packs & Determinations
-- (ZS-DATA-001 §14, Invariants D06, D07, D09)
--
-- DOCTRINE:
-- 1. Tax Rule Reproducibility (Scenario A4): A tax result is not complete unless the platform
--    can reproduce the applicable jurisdiction context, frozen rule-pack version, individual rules,
--    exact rates (NUMERIC(38,18)) and input facts hash.
-- 2. Traceability: Invoices link to tax determinations, which link to tax components and specific rules.
-- 3. Row-Level Security: Strict tenant isolation enforced for zoiko_backend role.

CREATE SCHEMA IF NOT EXISTS tax_management;

COMMENT ON SCHEMA tax_management IS
    'Canonical Tax Engine. Tax registrations, versioned rule packs, individual tax rules, and immutable tax determinations.';

GRANT USAGE ON SCHEMA tax_management TO zoiko_backend, authenticated;

-- ── tax_registrations (TAX-REG) ───────────────────────────────────────────────

CREATE TABLE tax_management.tax_registrations (
    tax_registration_id                 UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    legal_entity_id                     UUID          NOT NULL,
    establishment_id                    UUID,
    jurisdiction_id                     UUID          NOT NULL,
    tax_type                            VARCHAR(30)   NOT NULL, -- 'VAT' | 'GST' | 'SALES_TAX' | 'INCOME_TAX' | 'WITHHOLDING'
    registration_number_ref             TEXT          NOT NULL, -- Encrypted vault ref
    masked_number                       VARCHAR(100),
    effective_from                      DATE          NOT NULL,
    effective_to                        DATE,
    filing_frequency                    VARCHAR(20),  -- 'MONTHLY' | 'QUARTERLY' | 'ANNUAL'
    status                              VARCHAR(20)   NOT NULL DEFAULT 'ACTIVE', -- 'PENDING' | 'ACTIVE' | 'SUSPENDED' | 'CANCELLED'
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_tax_reg_type CHECK (tax_type IN ('VAT', 'GST', 'SALES_TAX', 'INCOME_TAX', 'WITHHOLDING')),
    CONSTRAINT chk_tax_reg_status CHECK (status IN ('PENDING', 'ACTIVE', 'SUSPENDED', 'CANCELLED')),
    CONSTRAINT chk_tax_reg_dates CHECK (effective_to IS NULL OR effective_to >= effective_from)
);

CREATE INDEX idx_tax_reg_tenant_entity ON tax_management.tax_registrations (tenant_id, legal_entity_id, tax_type);

ALTER TABLE tax_management.tax_registrations ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_management.tax_registrations FORCE ROW LEVEL SECURITY;

CREATE POLICY tax_reg_tenant_isolation ON tax_management.tax_registrations
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON tax_management.tax_registrations TO zoiko_backend, authenticated;

-- ── tax_rule_packs (TAX-PACK) ─────────────────────────────────────────────────

CREATE TABLE tax_management.tax_rule_packs (
    tax_rule_pack_id                    UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_id                     UUID          NOT NULL,
    tax_type                            VARCHAR(30)   NOT NULL,
    pack_version                        VARCHAR(30)   NOT NULL, -- SemVer (e.g. '2026.1.0')
    effective_from                      DATE          NOT NULL,
    effective_to                        DATE,
    source_authority_ref                TEXT          NOT NULL,
    approval_status                     VARCHAR(20)   NOT NULL DEFAULT 'DRAFT', -- 'DRAFT' | 'VALIDATED' | 'APPROVED' | 'RETIRED'
    content_hash                        VARCHAR(64)   NOT NULL, -- SHA-256
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_pack_status CHECK (approval_status IN ('DRAFT', 'VALIDATED', 'APPROVED', 'RETIRED')),
    CONSTRAINT chk_pack_dates CHECK (effective_to IS NULL OR effective_to >= effective_from),
    CONSTRAINT uq_jur_pack_version UNIQUE (jurisdiction_id, tax_type, pack_version)
);

CREATE INDEX idx_tax_packs_jur_dates ON tax_management.tax_rule_packs (jurisdiction_id, tax_type, effective_from);

-- ── tax_rules (TAX-RULE) ─────────────────────────────────────────────────────

CREATE TABLE tax_management.tax_rules (
    tax_rule_id                         UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tax_rule_pack_id                    UUID          NOT NULL,
    rule_code                           VARCHAR(50)   NOT NULL,
    rule_type                           VARCHAR(30)   NOT NULL, -- 'STANDARD_RATE' | 'REDUCED_RATE' | 'EXEMPTION' | 'ZERO_RATED'
    rate                                NUMERIC(38,18) NOT NULL DEFAULT 0.000000000000000000,
    condition_expression                JSONB,
    result_expression                   JSONB,
    effective_from                      DATE          NOT NULL,
    effective_to                        DATE,

    CONSTRAINT uq_pack_rule_code UNIQUE (tax_rule_pack_id, rule_code),
    CONSTRAINT chk_rule_dates CHECK (effective_to IS NULL OR effective_to >= effective_from),
    CONSTRAINT fk_tax_rules_pack FOREIGN KEY (tax_rule_pack_id) REFERENCES tax_management.tax_rule_packs(tax_rule_pack_id) ON DELETE CASCADE
);

CREATE INDEX idx_tax_rules_pack ON tax_management.tax_rules (tax_rule_pack_id);

-- ── tax_determinations (TAX-DET) ──────────────────────────────────────────────

CREATE TABLE tax_management.tax_determinations (
    tax_determination_id                UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    legal_entity_id                     UUID          NOT NULL,
    source_object_table                 VARCHAR(50)   NOT NULL,
    source_object_id                    UUID          NOT NULL,
    rule_pack_version_id                UUID          NOT NULL, -- Lineage to frozen TaxRulePack
    tax_point_date                      DATE          NOT NULL,
    taxable_basis                       NUMERIC(38,12) NOT NULL,
    currency_code                       VARCHAR(3)    NOT NULL,
    result_status                       VARCHAR(20)   NOT NULL DEFAULT 'CALCULATED', -- 'CALCULATED' | 'EXEMPT' | 'OUT_OF_SCOPE' | 'MANUAL_REVIEW' | 'OVERRIDDEN'
    input_facts_hash                    VARCHAR(64)   NOT NULL, -- SHA-256
    override_ref                        UUID,
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_det_status CHECK (result_status IN ('CALCULATED', 'EXEMPT', 'OUT_OF_SCOPE', 'MANUAL_REVIEW', 'OVERRIDDEN')),
    CONSTRAINT fk_det_rule_pack FOREIGN KEY (rule_pack_version_id) REFERENCES tax_management.tax_rule_packs(tax_rule_pack_id) ON DELETE RESTRICT
);

CREATE INDEX idx_tax_det_tenant_source ON tax_management.tax_determinations (tenant_id, source_object_table, source_object_id);
CREATE INDEX idx_tax_det_tenant_point ON tax_management.tax_determinations (tenant_id, tax_point_date);

ALTER TABLE tax_management.tax_determinations ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_management.tax_determinations FORCE ROW LEVEL SECURITY;

CREATE POLICY tax_det_tenant_isolation ON tax_management.tax_determinations
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON tax_management.tax_determinations TO zoiko_backend, authenticated;

-- ── tax_components (TAX-COMP) ────────────────────────────────────────────────

CREATE TABLE tax_management.tax_components (
    tax_component_id                    UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    tax_determination_id                UUID          NOT NULL,
    tax_type                            VARCHAR(30)   NOT NULL,
    jurisdiction_id                     UUID          NOT NULL,
    rate                                NUMERIC(38,18) NOT NULL,
    taxable_amount                      NUMERIC(38,12) NOT NULL,
    tax_amount                          NUMERIC(38,12) NOT NULL,
    tax_code                            VARCHAR(50),
    rule_id                             UUID          NOT NULL,

    CONSTRAINT fk_comp_det FOREIGN KEY (tax_determination_id) REFERENCES tax_management.tax_determinations(tax_determination_id) ON DELETE CASCADE,
    CONSTRAINT fk_comp_rule FOREIGN KEY (rule_id) REFERENCES tax_management.tax_rules(tax_rule_id) ON DELETE RESTRICT
);

CREATE INDEX idx_tax_comp_tenant_det ON tax_management.tax_components (tenant_id, tax_determination_id);

ALTER TABLE tax_management.tax_components ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_management.tax_components FORCE ROW LEVEL SECURITY;

CREATE POLICY tax_comp_tenant_isolation ON tax_management.tax_components
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON tax_management.tax_components TO zoiko_backend, authenticated;
