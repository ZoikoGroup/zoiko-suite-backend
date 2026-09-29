-- =============================================================================
-- Migration: 0048_governance_disposition_and_archive.sql
-- Description: Canonical Schema for Defensible Disposition Batches, Destruction
--              Certificates, Archive Packages, Certifications, Purpose Use, and Transfers.
-- Compliance: ZS-DATA-GOV-001 §19, §20, §21, §23, §24, §25, §28
-- Controls: DG-041, DG-042, DG-043, DG-044, DG-045, DG-046, DG-047, DG-048,
--           DG-049, DG-050, DG-054, DG-055, DG-056, DG-057, DG-058, DG-059, DG-060
-- Invariants: GOV-19, GOV-20, GOV-21, GOV-22, GOV-23, GOV-24, GOV-25
--             NP-20 through NP-36
-- Tenancy: RLS enabled + forced on all tables; fail-closed zoiko_backend role
-- =============================================================================

CREATE SCHEMA IF NOT EXISTS governance_archive;

-- -----------------------------------------------------------------------------
-- 1. Disposition Batches (ZS-DATA-GOV-001 §19.1, §28, DG-041, DG-042, NP-20, NP-21)
-- Frozen population under multi-phase governance approval and execution
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_disposition.disposition_batches (
    batch_id                UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    batch_code              VARCHAR(64) NOT NULL,
    population_snapshot_hash CHAR(64) NOT NULL,
    record_count            INTEGER NOT NULL,
    execution_method        VARCHAR(32) NOT NULL,
    policy_versions_applied TEXT NOT NULL,
    eligibility_evidence    TEXT NOT NULL,
    exceptions              TEXT NULL,
    status                  VARCHAR(32) NOT NULL DEFAULT 'PENDING_APPROVAL',
    approver_principal_id   VARCHAR(128) NULL,
    executor_principal_id   VARCHAR(128) NULL,
    backup_tombstone_digest CHAR(64) NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    approved_at             TIMESTAMPTZ NULL,
    executed_at             TIMESTAMPTZ NULL,
    CONSTRAINT uq_disposition_batches_code UNIQUE (tenant_id, batch_code),
    CONSTRAINT chk_disposition_batches_method CHECK (execution_method IN ('SECURE_PURGE', 'CRYPTO_SHRED', 'ANONYMIZE', 'ARCHIVE_MOVE')),
    CONSTRAINT chk_disposition_batches_status CHECK (status IN ('PENDING_APPROVAL', 'APPROVED', 'EXECUTED', 'PARTIALLY_FAILED', 'REJECTED')),
    CONSTRAINT chk_disposition_batches_count CHECK (record_count > 0)
);

CREATE INDEX IF NOT EXISTS idx_disposition_batches_status
    ON governance_disposition.disposition_batches (tenant_id, status);

-- -----------------------------------------------------------------------------
-- 2. Destruction Certificates (ZS-DATA-GOV-001 §19.1, §28, DG-046, NP-34)
-- Cryptographic proof of verified and completed lawful destruction
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_disposition.destruction_certificates (
    certificate_id          UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    batch_id                UUID NOT NULL REFERENCES governance_disposition.disposition_batches(batch_id),
    certificate_number      VARCHAR(128) NOT NULL,
    method                  VARCHAR(32) NOT NULL,
    verified_purged_count   INTEGER NOT NULL,
    certificate_digest_sha256 CHAR(64) NOT NULL,
    issued_at               TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    issued_by               VARCHAR(128) NOT NULL,
    CONSTRAINT uq_destruction_certificates_number UNIQUE (tenant_id, certificate_number),
    CONSTRAINT chk_destruction_certs_count CHECK (verified_purged_count > 0)
);

CREATE INDEX IF NOT EXISTS idx_destruction_certs_batch
    ON governance_disposition.destruction_certificates (tenant_id, batch_id);

-- -----------------------------------------------------------------------------
-- 3. Archive Packages (ZS-DATA-GOV-001 §21, §28, DG-047..DG-050, NP-26)
-- Immutable sealed archive units with manifest and policy bindings
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_archive.archive_packages (
    archive_id              UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    archive_code            VARCHAR(64) NOT NULL,
    record_count            INTEGER NOT NULL,
    total_bytes             BIGINT NOT NULL,
    package_digest_sha256   CHAR(64) NOT NULL,
    retention_schedule_ref  VARCHAR(128) NOT NULL,
    hold_bindings           TEXT NULL,
    status                  VARCHAR(32) NOT NULL DEFAULT 'SEALED',
    sealed_at               TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    sealed_by               VARCHAR(128) NOT NULL,
    CONSTRAINT uq_archive_packages_code UNIQUE (tenant_id, archive_code),
    CONSTRAINT chk_archive_packages_status CHECK (status IN ('SEALED', 'RESTORED', 'TAMPERED')),
    CONSTRAINT chk_archive_packages_counts CHECK (record_count > 0 AND total_bytes >= 0)
);

CREATE INDEX IF NOT EXISTS idx_archive_packages_status
    ON governance_archive.archive_packages (tenant_id, status);

-- -----------------------------------------------------------------------------
-- 4. Data Certifications (ZS-DATA-GOV-001 §23, §28, DG-057, DG-058, NP-30)
-- Formal data certification levels C0 through C4
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_archive.data_certifications (
    certification_id        UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    asset_id                UUID NOT NULL,
    cert_class              VARCHAR(32) NOT NULL,
    evidence_manifest_id    UUID NULL,
    dq_run_id               UUID NULL,
    lineage_hash            VARCHAR(128) NULL,
    status                  VARCHAR(32) NOT NULL DEFAULT 'EFFECTIVE',
    certified_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    certified_by            VARCHAR(128) NOT NULL,
    expires_at              TIMESTAMPTZ NOT NULL,
    CONSTRAINT chk_data_certifications_class CHECK (cert_class IN ('C0_UNCERTIFIED', 'C1_OPERATIONAL', 'C2_CONTROLLED', 'C3_FINANCIAL_REGULATORY', 'C4_LEGAL_EVIDENTIARY')),
    CONSTRAINT chk_data_certifications_status CHECK (status IN ('EFFECTIVE', 'EXPIRED', 'INVALIDATED')),
    CONSTRAINT chk_data_certifications_expiry CHECK (expires_at > certified_at)
);

CREATE INDEX IF NOT EXISTS idx_data_certifications_asset
    ON governance_archive.data_certifications (tenant_id, asset_id, status);

-- -----------------------------------------------------------------------------
-- 5. Data Use Authorizations (ZS-DATA-GOV-001 §24, §28, DG-008, DG-055, NP-19)
-- Permitted secondary and consumer purpose authorizations
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_archive.data_use_authorizations (
    authorization_id        UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    asset_id                UUID NOT NULL,
    consumer_id             VARCHAR(128) NOT NULL,
    approved_purpose        VARCHAR(64) NOT NULL,
    status                  VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',
    valid_from              TIMESTAMPTZ NOT NULL,
    valid_to                TIMESTAMPTZ NULL,
    CONSTRAINT chk_data_use_auth_status CHECK (status IN ('ACTIVE', 'REVOKED')),
    CONSTRAINT chk_data_use_auth_dates CHECK (valid_to IS NULL OR valid_to > valid_from)
);

CREATE INDEX IF NOT EXISTS idx_data_use_auth_lookup
    ON governance_archive.data_use_authorizations (tenant_id, asset_id, consumer_id, status);

-- -----------------------------------------------------------------------------
-- 6. Data Transfer Records (ZS-DATA-GOV-001 §28, NP-29)
-- Evidence of cross-region and cross-jurisdiction data transfers
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_archive.data_transfers (
    transfer_id             UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    asset_id                UUID NOT NULL,
    source_region           VARCHAR(64) NOT NULL,
    target_region           VARCHAR(64) NOT NULL,
    recipient               VARCHAR(128) NOT NULL,
    lawful_basis            VARCHAR(128) NOT NULL,
    is_registered           BOOLEAN NOT NULL DEFAULT TRUE,
    transferred_at          TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS idx_data_transfers_asset
    ON governance_archive.data_transfers (tenant_id, asset_id, transferred_at DESC);

-- -----------------------------------------------------------------------------
-- Row-Level Security Policies (Multi-Tenant Fail-Closed)
-- -----------------------------------------------------------------------------

ALTER TABLE governance_disposition.disposition_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_disposition.disposition_batches FORCE ROW LEVEL SECURITY;
CREATE POLICY disposition_batches_tenant_isolation ON governance_disposition.disposition_batches
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_disposition.destruction_certificates ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_disposition.destruction_certificates FORCE ROW LEVEL SECURITY;
CREATE POLICY destruction_certificates_tenant_isolation ON governance_disposition.destruction_certificates
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_archive.archive_packages ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_archive.archive_packages FORCE ROW LEVEL SECURITY;
CREATE POLICY archive_packages_tenant_isolation ON governance_archive.archive_packages
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_archive.data_certifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_archive.data_certifications FORCE ROW LEVEL SECURITY;
CREATE POLICY data_certifications_tenant_isolation ON governance_archive.data_certifications
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_archive.data_use_authorizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_archive.data_use_authorizations FORCE ROW LEVEL SECURITY;
CREATE POLICY data_use_authorizations_tenant_isolation ON governance_archive.data_use_authorizations
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_archive.data_transfers ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_archive.data_transfers FORCE ROW LEVEL SECURITY;
CREATE POLICY data_transfers_tenant_isolation ON governance_archive.data_transfers
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

-- Grant schema access to backend role
GRANT USAGE ON SCHEMA governance_archive TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA governance_disposition TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA governance_archive TO zoiko_backend;
