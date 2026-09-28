-- =============================================================================
-- Migration: 0044_governance_registry_and_ownership.sql
-- Description: Canonical Schema for Governance Registry, Ownership, Stewardship,
--              Custody, Contracts and Data Classification Bindings.
-- Compliance: ZS-DATA-GOV-001 §3, §4, §5, §28 (Governance Registry Foundation)
-- Controls: DG-001, DG-002, DG-003, DG-004, DG-005, DG-006, DG-007, DG-009, DG-010
-- Invariants: GOV-01, GOV-02, GOV-03, GOV-04, GOV-05, NP-01, NP-02
-- Tenancy: RLS enabled + forced on all tables; fail-closed zoiko_backend role
-- =============================================================================

CREATE SCHEMA IF NOT EXISTS governance_registry;

-- -----------------------------------------------------------------------------
-- 1. Data Domains (ZS-DATA-GOV-001 §28)
-- Logical business/data domain and ownership boundary
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_registry.data_domains (
    domain_id               UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    domain_code             VARCHAR(64) NOT NULL,
    domain_name             VARCHAR(128) NOT NULL,
    description             TEXT NULL,
    status                  VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT uq_data_domains_tenant_code UNIQUE (tenant_id, domain_code),
    CONSTRAINT chk_data_domains_status CHECK (status IN ('ACTIVE', 'INACTIVE', 'DEPRECATED'))
);

CREATE INDEX IF NOT EXISTS idx_data_domains_tenant_status
    ON governance_registry.data_domains (tenant_id, status);

-- -----------------------------------------------------------------------------
-- 2. Data Assets (ZS-DATA-GOV-001 §28, §4.1)
-- Governed dataset, entity, record population or derived data product
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_registry.data_assets (
    asset_id                UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    domain_id               UUID NOT NULL REFERENCES governance_registry.data_domains(domain_id),
    asset_code              VARCHAR(64) NOT NULL,
    asset_name              VARCHAR(128) NOT NULL,
    asset_type              VARCHAR(32) NOT NULL,
    criticality_tier        VARCHAR(32) NOT NULL,
    is_authoritative        BOOLEAN NOT NULL DEFAULT TRUE,
    authoritative_service   VARCHAR(128) NOT NULL,
    storage_location        VARCHAR(255) NOT NULL,
    legal_entity_id         UUID NULL,
    accounting_book_id      UUID NULL,
    jurisdiction_code       VARCHAR(16) NULL,
    business_process        VARCHAR(128) NOT NULL,
    status                  VARCHAR(32) NOT NULL DEFAULT 'REGISTERED',
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT uq_data_assets_tenant_code UNIQUE (tenant_id, asset_code),
    CONSTRAINT chk_data_assets_type CHECK (asset_type IN ('TABLE', 'DATASET', 'EVENT_STREAM', 'REPORT', 'DERIVED_PRODUCT')),
    CONSTRAINT chk_data_assets_criticality CHECK (criticality_tier IN ('TIER_1_CRITICAL', 'TIER_2_OPERATIONAL', 'TIER_3_SUPPORTING')),
    CONSTRAINT chk_data_assets_status CHECK (status IN ('REGISTERED', 'CERTIFIED', 'DEPRECATED'))
);

CREATE INDEX IF NOT EXISTS idx_data_assets_tenant_domain
    ON governance_registry.data_assets (tenant_id, domain_id);

CREATE INDEX IF NOT EXISTS idx_data_assets_tenant_criticality
    ON governance_registry.data_assets (tenant_id, criticality_tier);

CREATE INDEX IF NOT EXISTS idx_data_assets_tenant_service
    ON governance_registry.data_assets (tenant_id, authoritative_service);

-- -----------------------------------------------------------------------------
-- 3. Data Owner Assignments (ZS-DATA-GOV-001 §28, DG-001, DG-010)
-- Effective-dated accountable business/domain ownership
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_registry.data_owner_assignments (
    assignment_id           UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    asset_id                UUID NOT NULL REFERENCES governance_registry.data_assets(asset_id),
    owner_principal_id      VARCHAR(128) NOT NULL,
    owner_role              VARCHAR(64) NOT NULL,
    effective_from          TIMESTAMPTZ NOT NULL,
    effective_to            TIMESTAMPTZ NULL,
    approved_by             VARCHAR(128) NOT NULL,
    assignment_evidence_id  UUID NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT chk_data_owner_effective_dates CHECK (effective_to IS NULL OR effective_to > effective_from)
);

CREATE INDEX IF NOT EXISTS idx_data_owner_assignments_lookup
    ON governance_registry.data_owner_assignments (tenant_id, asset_id, effective_from);

-- -----------------------------------------------------------------------------
-- 4. Stewardship Assignments (ZS-DATA-GOV-001 §28, DG-002)
-- Effective-dated operational stewardship
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_registry.stewardship_assignments (
    assignment_id           UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    asset_id                UUID NOT NULL REFERENCES governance_registry.data_assets(asset_id),
    steward_principal_id    VARCHAR(128) NOT NULL,
    operational_unit        VARCHAR(128) NOT NULL,
    effective_from          TIMESTAMPTZ NOT NULL,
    effective_to            TIMESTAMPTZ NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT chk_stewardship_effective_dates CHECK (effective_to IS NULL OR effective_to > effective_from)
);

CREATE INDEX IF NOT EXISTS idx_stewardship_assignments_lookup
    ON governance_registry.stewardship_assignments (tenant_id, asset_id, effective_from);

-- -----------------------------------------------------------------------------
-- 5. Custodian Assignments (ZS-DATA-GOV-001 §28, DG-003)
-- Physical/platform custody assignment (separated from business ownership)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_registry.custodian_assignments (
    assignment_id           UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    asset_id                UUID NOT NULL REFERENCES governance_registry.data_assets(asset_id),
    custodian_system_or_team VARCHAR(128) NOT NULL,
    infrastructure_provider VARCHAR(64) NOT NULL,
    effective_from          TIMESTAMPTZ NOT NULL,
    effective_to            TIMESTAMPTZ NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT chk_custodian_effective_dates CHECK (effective_to IS NULL OR effective_to > effective_from)
);

CREATE INDEX IF NOT EXISTS idx_custodian_assignments_lookup
    ON governance_registry.custodian_assignments (tenant_id, asset_id, effective_from);

-- -----------------------------------------------------------------------------
-- 6. Data Contracts (ZS-DATA-GOV-001 §28, DG-006)
-- Canonical schema, interface, and semantic contract
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_registry.data_contracts (
    contract_id             UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    asset_id                UUID NOT NULL REFERENCES governance_registry.data_assets(asset_id),
    version                 VARCHAR(32) NOT NULL,
    schema_definition       TEXT NOT NULL,
    compatibility_mode      VARCHAR(32) NOT NULL DEFAULT 'BACKWARD',
    status                  VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',
    effective_from          TIMESTAMPTZ NOT NULL,
    effective_to            TIMESTAMPTZ NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT uq_data_contracts_asset_version UNIQUE (tenant_id, asset_id, version),
    CONSTRAINT chk_data_contracts_compatibility CHECK (compatibility_mode IN ('BACKWARD', 'FULL', 'NONE')),
    CONSTRAINT chk_data_contracts_status CHECK (status IN ('DRAFT', 'ACTIVE', 'SUPERSEDED'))
);

CREATE INDEX IF NOT EXISTS idx_data_contracts_lookup
    ON governance_registry.data_contracts (tenant_id, asset_id, effective_from);

-- -----------------------------------------------------------------------------
-- 7. Data Classification Bindings (ZS-DATA-GOV-001 §28, DG-007, DG-008)
-- Security, privacy, and regulatory classification metadata
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_registry.data_classification_bindings (
    binding_id              UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    asset_id                UUID NOT NULL REFERENCES governance_registry.data_assets(asset_id),
    sensitivity_level       VARCHAR(32) NOT NULL,
    confidentiality_class   VARCHAR(32) NOT NULL DEFAULT 'STANDARD',
    privacy_class           VARCHAR(32) NOT NULL DEFAULT 'NONE',
    regulatory_regime       VARCHAR(64) NOT NULL,
    retention_profile_ref   VARCHAR(128) NOT NULL,
    purpose_restriction_ref VARCHAR(128) NULL,
    effective_from          TIMESTAMPTZ NOT NULL,
    effective_to            TIMESTAMPTZ NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT chk_classification_sensitivity CHECK (sensitivity_level IN ('PUBLIC', 'INTERNAL', 'CONFIDENTIAL', 'RESTRICTED')),
    CONSTRAINT chk_classification_effective_dates CHECK (effective_to IS NULL OR effective_to > effective_from)
);

CREATE INDEX IF NOT EXISTS idx_classification_bindings_lookup
    ON governance_registry.data_classification_bindings (tenant_id, asset_id, effective_from);

-- -----------------------------------------------------------------------------
-- Row-Level Security Policies (Multi-Tenant Fail-Closed)
-- -----------------------------------------------------------------------------

ALTER TABLE governance_registry.data_domains ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_registry.data_domains FORCE ROW LEVEL SECURITY;
CREATE POLICY data_domains_tenant_isolation ON governance_registry.data_domains
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_registry.data_assets ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_registry.data_assets FORCE ROW LEVEL SECURITY;
CREATE POLICY data_assets_tenant_isolation ON governance_registry.data_assets
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_registry.data_owner_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_registry.data_owner_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY data_owner_assignments_tenant_isolation ON governance_registry.data_owner_assignments
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_registry.stewardship_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_registry.stewardship_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY stewardship_assignments_tenant_isolation ON governance_registry.stewardship_assignments
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_registry.custodian_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_registry.custodian_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY custodian_assignments_tenant_isolation ON governance_registry.custodian_assignments
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_registry.data_contracts ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_registry.data_contracts FORCE ROW LEVEL SECURITY;
CREATE POLICY data_contracts_tenant_isolation ON governance_registry.data_contracts
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_registry.data_classification_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_registry.data_classification_bindings FORCE ROW LEVEL SECURITY;
CREATE POLICY data_classification_bindings_tenant_isolation ON governance_registry.data_classification_bindings
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

-- Grant schema access to backend role
GRANT USAGE ON SCHEMA governance_registry TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA governance_registry TO zoiko_backend;
