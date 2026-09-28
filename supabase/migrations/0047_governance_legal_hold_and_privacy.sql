-- =============================================================================
-- Migration: 0047_governance_legal_hold_and_privacy.sql
-- Description: Canonical Schema for Legal Hold Directives, Versioned Scopes,
--              Prospective Capture, and Privacy Conflict Resolution.
-- Compliance: ZS-DATA-GOV-001 §16, §17, §18, §28 (Legal Hold & Privacy Resolution)
-- Controls: DG-036, DG-037, DG-038, DG-039, DG-040, DG-051, DG-052, DG-053
-- Invariants: GOV-16, GOV-17, GOV-18, NP-13, NP-14, NP-15, NP-16, NP-17, NP-18, NP-19
-- Tenancy: RLS enabled + forced on all tables; fail-closed zoiko_backend role
-- =============================================================================

CREATE SCHEMA IF NOT EXISTS governance_disposition;

-- -----------------------------------------------------------------------------
-- 1. Legal Holds (ZS-DATA-GOV-001 §16, §28, DG-036, DG-039, NP-13, NP-16)
-- Authorized preservation directives and segregation-of-duties release governance
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_disposition.legal_holds (
    hold_id                 UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    hold_matter_code        VARCHAR(64) NOT NULL,
    authority_description  TEXT NOT NULL,
    issued_by               VARCHAR(128) NOT NULL,
    issued_at               TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    status                  VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',
    released_by             VARCHAR(128) NULL,
    release_approved_by     VARCHAR(128) NULL,
    released_at             TIMESTAMPTZ NULL,
    release_justification   TEXT NULL,
    CONSTRAINT uq_legal_holds_matter UNIQUE (tenant_id, hold_matter_code),
    CONSTRAINT chk_legal_holds_status CHECK (status IN ('ACTIVE', 'RELEASED'))
);

CREATE INDEX IF NOT EXISTS idx_legal_holds_tenant_status
    ON governance_disposition.legal_holds (tenant_id, status);

-- -----------------------------------------------------------------------------
-- 2. Legal Hold Scopes (ZS-DATA-GOV-001 §16, §28, DG-037, DG-038, NP-14, NP-15)
-- Versioned scope definitions with prospective record capture
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_disposition.legal_hold_scopes (
    scope_id                UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    hold_id                 UUID NOT NULL REFERENCES governance_disposition.legal_holds(hold_id),
    version                 INTEGER NOT NULL DEFAULT 1,
    target_entity_types     TEXT NOT NULL,
    custodian_principal_ids TEXT NOT NULL DEFAULT '[]',
    date_range_start        TIMESTAMPTZ NULL,
    date_range_end          TIMESTAMPTZ NULL,
    is_prospective          BOOLEAN NOT NULL DEFAULT FALSE,
    filter_predicate        TEXT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_by              VARCHAR(128) NOT NULL,
    CONSTRAINT uq_legal_hold_scopes_version UNIQUE (tenant_id, hold_id, version)
);

CREATE INDEX IF NOT EXISTS idx_legal_hold_scopes_hold
    ON governance_disposition.legal_hold_scopes (tenant_id, hold_id, version DESC);

-- -----------------------------------------------------------------------------
-- 3. Privacy Disposition Requests (ZS-DATA-GOV-001 §18, §28, DG-040)
-- Data subject erasure, restriction, or rectification requests
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_disposition.privacy_requests (
    request_id              UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    data_subject_id         VARCHAR(128) NOT NULL,
    request_type            VARCHAR(32) NOT NULL,
    requested_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    status                  VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    CONSTRAINT chk_privacy_requests_type CHECK (request_type IN ('ERASURE', 'RESTRICTION', 'RECTIFY')),
    CONSTRAINT chk_privacy_requests_status CHECK (status IN ('PENDING', 'RESOLVED', 'REJECTED'))
);

CREATE INDEX IF NOT EXISTS idx_privacy_requests_subject
    ON governance_disposition.privacy_requests (tenant_id, data_subject_id, status);

-- -----------------------------------------------------------------------------
-- 4. Privacy Resolutions (ZS-DATA-GOV-001 §18, §28, DG-040, DG-052, NP-17, NP-18)
-- Recorded conflict resolutions (ERASE, ANONYMIZE, PARTIAL, DEFER, DENY_WITH_BASIS)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_disposition.privacy_resolutions (
    resolution_id           UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    request_id              UUID NOT NULL REFERENCES governance_disposition.privacy_requests(request_id),
    outcome                 VARCHAR(32) NOT NULL,
    legal_regulatory_basis  VARCHAR(255) NOT NULL,
    hold_block_ref          VARCHAR(128) NULL,
    field_actions           TEXT NULL,
    review_date             TIMESTAMPTZ NULL,
    resolved_at             TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    resolved_by             VARCHAR(128) NOT NULL,
    CONSTRAINT chk_privacy_resolutions_outcome CHECK (outcome IN ('ERASE', 'ANONYMIZE', 'PARTIAL', 'DEFER', 'DENY_WITH_BASIS'))
);

CREATE INDEX IF NOT EXISTS idx_privacy_resolutions_request
    ON governance_disposition.privacy_resolutions (tenant_id, request_id);

-- -----------------------------------------------------------------------------
-- Row-Level Security Policies (Multi-Tenant Fail-Closed)
-- -----------------------------------------------------------------------------

ALTER TABLE governance_disposition.legal_holds ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_disposition.legal_holds FORCE ROW LEVEL SECURITY;
CREATE POLICY legal_holds_tenant_isolation ON governance_disposition.legal_holds
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_disposition.legal_hold_scopes ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_disposition.legal_hold_scopes FORCE ROW LEVEL SECURITY;
CREATE POLICY legal_hold_scopes_tenant_isolation ON governance_disposition.legal_hold_scopes
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_disposition.privacy_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_disposition.privacy_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY privacy_requests_tenant_isolation ON governance_disposition.privacy_requests
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_disposition.privacy_resolutions ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_disposition.privacy_resolutions FORCE ROW LEVEL SECURITY;
CREATE POLICY privacy_resolutions_tenant_isolation ON governance_disposition.privacy_resolutions
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

-- Grant schema access to backend role
GRANT USAGE ON SCHEMA governance_disposition TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA governance_disposition TO zoiko_backend;
