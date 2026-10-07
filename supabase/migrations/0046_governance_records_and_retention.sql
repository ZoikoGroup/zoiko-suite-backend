-- =============================================================================
-- Migration: 0046_governance_records_and_retention.sql
-- Description: Canonical Schema for Evidence Sealing, Chain of Custody,
--              Record Declarations, and Effective-Dated Retention Schedules.
-- Compliance: ZS-DATA-GOV-001 §10, §11, §14, §15, §22, §28 (Evidence & Records Engine)
-- Controls: DG-028, DG-029, DG-030, DG-031, DG-032, DG-033, DG-034, DG-035
-- Invariants: GOV-11, GOV-12, GOV-13, GOV-14, GOV-15, NP-09, NP-10, NP-11, NP-12
-- Tenancy: RLS enabled + forced on all tables; fail-closed zoiko_backend role
-- =============================================================================

CREATE SCHEMA IF NOT EXISTS governance_evidence;
CREATE SCHEMA IF NOT EXISTS governance_records;

-- -----------------------------------------------------------------------------
-- 1. Evidence Objects (ZS-DATA-GOV-001 §10, §28, DG-028, DG-029)
-- Immutable, hash-verifiable evidence artifacts directly bound to business subject
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_evidence.evidence_objects (
    object_id               UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    subject_entity_type     VARCHAR(64) NOT NULL,
    subject_entity_id       UUID NOT NULL,
    object_type             VARCHAR(32) NOT NULL,
    mime_type               VARCHAR(64) NOT NULL,
    content_hash_sha256     CHAR(64) NOT NULL,
    storage_uri             VARCHAR(255) NOT NULL,
    byte_size               BIGINT NOT NULL,
    sealed_at               TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_by              VARCHAR(128) NOT NULL,
    CONSTRAINT chk_evidence_objects_size CHECK (byte_size >= 0)
);

CREATE INDEX IF NOT EXISTS idx_evidence_objects_subject
    ON governance_evidence.evidence_objects (tenant_id, subject_entity_type, subject_entity_id);

CREATE INDEX IF NOT EXISTS idx_evidence_objects_hash
    ON governance_evidence.evidence_objects (tenant_id, content_hash_sha256);

-- -----------------------------------------------------------------------------
-- 2. Evidence Manifests (ZS-DATA-GOV-001 §10, §28, NP-09)
-- Sealed package binding multiple evidence objects and combined integrity digest
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_evidence.evidence_manifests (
    manifest_id             UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    manifest_code           VARCHAR(64) NOT NULL,
    package_type            VARCHAR(64) NOT NULL,
    object_count            INTEGER NOT NULL,
    total_bytes             BIGINT NOT NULL,
    manifest_digest_sha256  CHAR(64) NOT NULL,
    status                  VARCHAR(32) NOT NULL DEFAULT 'SEALED',
    sealed_at               TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    sealed_by               VARCHAR(128) NOT NULL,
    CONSTRAINT uq_evidence_manifests_code UNIQUE (tenant_id, manifest_code),
    CONSTRAINT chk_evidence_manifests_status CHECK (status IN ('OPEN', 'SEALED', 'VERIFIED', 'TAMPERED')),
    CONSTRAINT chk_evidence_manifests_count CHECK (object_count >= 0 AND total_bytes >= 0)
);

CREATE INDEX IF NOT EXISTS idx_evidence_manifests_status
    ON governance_evidence.evidence_manifests (tenant_id, status);

-- -----------------------------------------------------------------------------
-- 3. Chain of Custody Events (ZS-DATA-GOV-001 §22, DG-030, NP-10)
-- Audit history of evidence sealing, access, export, and transfer
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_evidence.custody_events (
    event_id                UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    object_id               UUID NULL,
    manifest_id             UUID NULL,
    event_type              VARCHAR(32) NOT NULL,
    actor_principal_id      VARCHAR(128) NOT NULL,
    actor_role              VARCHAR(64) NOT NULL,
    purpose                 VARCHAR(255) NOT NULL,
    occurred_at             TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT chk_custody_events_type CHECK (event_type IN ('SEALED', 'ACCESSED', 'EXPORTED', 'TRANSFERRED', 'VERIFIED'))
);

CREATE INDEX IF NOT EXISTS idx_custody_events_manifest
    ON governance_evidence.custody_events (tenant_id, manifest_id, occurred_at);

-- -----------------------------------------------------------------------------
-- 4. Record Classes (ZS-DATA-GOV-001 §11, §28)
-- Canonical categories of managed records
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_records.record_classes (
    record_class_id         UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    class_code              VARCHAR(64) NOT NULL,
    class_name              VARCHAR(128) NOT NULL,
    description             TEXT NULL,
    default_retention_years INTEGER NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT uq_record_classes_tenant_code UNIQUE (tenant_id, class_code),
    CONSTRAINT chk_record_classes_years CHECK (default_retention_years >= 0)
);

-- -----------------------------------------------------------------------------
-- 5. Record Declarations (ZS-DATA-GOV-001 §11, §28, DG-031, DG-032, NP-11)
-- Binding of business objects to immutable record controls
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_records.record_declarations (
    declaration_id          UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    record_class_id         UUID NOT NULL REFERENCES governance_records.record_classes(record_class_id),
    business_object_type    VARCHAR(64) NOT NULL,
    business_object_id      UUID NOT NULL,
    declaration_trigger     VARCHAR(32) NOT NULL,
    declared_at             TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    declared_by             VARCHAR(128) NOT NULL,
    is_immutable            BOOLEAN NOT NULL DEFAULT TRUE,
    CONSTRAINT uq_record_declarations_object UNIQUE (tenant_id, business_object_type, business_object_id),
    CONSTRAINT chk_record_declarations_trigger CHECK (declaration_trigger IN ('POSTED', 'FILED', 'CLOSED', 'EXECUTED'))
);

CREATE INDEX IF NOT EXISTS idx_record_declarations_class
    ON governance_records.record_declarations (tenant_id, record_class_id);

-- -----------------------------------------------------------------------------
-- 6. Retention Schedules (ZS-DATA-GOV-001 §14, §28, DG-033, NP-12)
-- Version-pinned, jurisdiction-aware retention schedules
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_records.retention_schedules (
    schedule_id             UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    record_class_id         UUID NOT NULL REFERENCES governance_records.record_classes(record_class_id),
    jurisdiction_code       VARCHAR(16) NOT NULL,
    legal_regulatory_basis  VARCHAR(255) NOT NULL,
    version                 INTEGER NOT NULL DEFAULT 1,
    min_retention_days      INTEGER NOT NULL,
    max_retention_days      INTEGER NULL,
    trigger_event_type      VARCHAR(64) NOT NULL,
    effective_from          TIMESTAMPTZ NOT NULL,
    effective_to            TIMESTAMPTZ NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT uq_retention_schedules_version UNIQUE (tenant_id, record_class_id, jurisdiction_code, version),
    CONSTRAINT chk_retention_schedule_days CHECK (min_retention_days >= 0),
    CONSTRAINT chk_retention_schedule_effective CHECK (effective_to IS NULL OR effective_to > effective_from)
);

CREATE INDEX IF NOT EXISTS idx_retention_schedules_lookup
    ON governance_records.retention_schedules (tenant_id, record_class_id, jurisdiction_code, effective_from);

-- -----------------------------------------------------------------------------
-- 7. Retention Triggers (ZS-DATA-GOV-001 §15, §28, DG-034, DG-035)
-- Authoritative event dates and calculated disposition eligibility
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_records.retention_triggers (
    trigger_id              UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    declaration_id          UUID NOT NULL REFERENCES governance_records.record_declarations(declaration_id),
    schedule_id             UUID NOT NULL REFERENCES governance_records.retention_schedules(schedule_id),
    schedule_version        INTEGER NOT NULL,
    trigger_date            TIMESTAMPTZ NOT NULL,
    eligible_disposition_date TIMESTAMPTZ NOT NULL,
    status                  VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    calculated_at           TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT uq_retention_triggers_declaration UNIQUE (tenant_id, declaration_id),
    CONSTRAINT chk_retention_triggers_status CHECK (status IN ('PENDING', 'ELIGIBILITY_REACHED', 'DISPOSITION_HELD', 'DISPOSED')),
    CONSTRAINT chk_retention_triggers_dates CHECK (eligible_disposition_date >= trigger_date)
);

CREATE INDEX IF NOT EXISTS idx_retention_triggers_status_date
    ON governance_records.retention_triggers (tenant_id, status, eligible_disposition_date);

-- -----------------------------------------------------------------------------
-- Row-Level Security Policies (Multi-Tenant Fail-Closed)
-- -----------------------------------------------------------------------------

ALTER TABLE governance_evidence.evidence_objects ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_evidence.evidence_objects FORCE ROW LEVEL SECURITY;
CREATE POLICY evidence_objects_tenant_isolation ON governance_evidence.evidence_objects
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_evidence.evidence_manifests ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_evidence.evidence_manifests FORCE ROW LEVEL SECURITY;
CREATE POLICY evidence_manifests_tenant_isolation ON governance_evidence.evidence_manifests
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_evidence.custody_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_evidence.custody_events FORCE ROW LEVEL SECURITY;
CREATE POLICY custody_events_tenant_isolation ON governance_evidence.custody_events
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_records.record_classes ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_records.record_classes FORCE ROW LEVEL SECURITY;
CREATE POLICY record_classes_tenant_isolation ON governance_records.record_classes
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_records.record_declarations ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_records.record_declarations FORCE ROW LEVEL SECURITY;
CREATE POLICY record_declarations_tenant_isolation ON governance_records.record_declarations
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_records.retention_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_records.retention_schedules FORCE ROW LEVEL SECURITY;
CREATE POLICY retention_schedules_tenant_isolation ON governance_records.retention_schedules
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_records.retention_triggers ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_records.retention_triggers FORCE ROW LEVEL SECURITY;
CREATE POLICY retention_triggers_tenant_isolation ON governance_records.retention_triggers
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

-- Grant schema access to backend role
GRANT USAGE ON SCHEMA governance_evidence TO zoiko_backend;
GRANT USAGE ON SCHEMA governance_records TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA governance_evidence TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA governance_records TO zoiko_backend;
