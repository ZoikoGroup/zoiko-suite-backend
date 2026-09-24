-- Migration: 000006_add_catalog.up.sql
--
-- BIZ-07 (Product & Service Catalog), from
-- ZoikoSuite_Business_Operations_Content_Services_Detailed_Service_Specifications_v1_0.docx.
-- Bolted on as a second domain beside INV-01..INV-05 in this service —
-- inventory_items.catalog_item_id (migration 000001) already anticipates
-- exactly this authority as an external reference it does not own.
--
-- Authoritative ownership (verbatim): "tenant/business offering identity,
-- descriptions, variants, units, eligibility/operational attributes and
-- version lifecycle." Explicitly NOT ZoikoSuite's own SaaS price book —
-- that is COM-01, owned by commercial-account-svc.
--
-- State model (verbatim, shared "Catalog" row): "Draft → Approved →
-- Active → Suspended/Retired → Superseded." Design decision made here:
-- Offering identity itself (sku_code, category, owner) is stable and has
-- no status of its own — every named lifecycle transition, and the
-- cross-cutting invariant "product/service offering versions used by
-- transactions SHALL be pinned; later catalog edits SHALL NOT rewrite
-- historical transaction meaning," belong to inventory_catalog_offering_versions
-- instead: content (description/unit/availability_rules) is set once at
-- CreateVersion and never edited in place (enforced below by
-- reject_catalog_version_content_mutation); only the lifecycle columns
-- move. Activate on a new version forces any prior ACTIVE version of the
-- same offering to SUPERSEDED in the same transaction — the real
-- mechanism behind OfferingVersionSuperseded and the one guarantee that
-- makes "historical transactions pin the version" true: a superseded
-- version row is never deleted or overwritten, only marked.
CREATE TABLE inventory_catalog_offerings (
    offering_id             UUID PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         VARCHAR(255) NOT NULL,
    sku_code                VARCHAR(100) NOT NULL,
    category                VARCHAR(100) NOT NULL,
    owner_principal_id      VARCHAR(255) NOT NULL,
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id VARCHAR(255) NOT NULL,

    UNIQUE (tenant_id, legal_entity_id, sku_code)
);

CREATE TABLE inventory_catalog_offering_versions (
    version_id                 UUID PRIMARY KEY,
    tenant_id                  VARCHAR(255) NOT NULL,
    offering_id                UUID NOT NULL REFERENCES inventory_catalog_offerings(offering_id),
    version_number              INT NOT NULL,
    description                 TEXT NOT NULL,
    unit                          VARCHAR(50) NOT NULL,
    availability_rules             TEXT,
    status                           VARCHAR(20) NOT NULL, -- DRAFT|APPROVED|ACTIVE|SUSPENDED|RETIRED|SUPERSEDED
    created_at                        TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id             VARCHAR(255) NOT NULL,
    approved_at                           TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id                VARCHAR(255),
    activated_at                              TIMESTAMP WITH TIME ZONE,
    activated_by_principal_id                   VARCHAR(255),
    suspended_at                                  TIMESTAMP WITH TIME ZONE,
    suspended_by_principal_id                       VARCHAR(255),
    suspension_reason                                 TEXT,
    retired_at                                          TIMESTAMP WITH TIME ZONE,
    retired_by_principal_id                               VARCHAR(255),
    retirement_reason                                       TEXT,
    superseded_at                                             TIMESTAMP WITH TIME ZONE,

    CONSTRAINT chk_catalog_version_status CHECK (status IN (
        'DRAFT', 'APPROVED', 'ACTIVE', 'SUSPENDED', 'RETIRED', 'SUPERSEDED'
    )),
    UNIQUE (tenant_id, offering_id, version_number)
);

-- "Historical transactions pin the offering version" — description, unit,
-- availability_rules, version_number and offering_id are the version's own
-- pinned content; once written they never change again, regardless of
-- application-code bugs. Every lifecycle column is deliberately excluded
-- from this guard (status and its timestamps are exactly what real
-- commands below are meant to change).
CREATE OR REPLACE FUNCTION reject_catalog_version_content_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.offering_id IS DISTINCT FROM OLD.offering_id OR
       NEW.version_number IS DISTINCT FROM OLD.version_number OR
       NEW.description IS DISTINCT FROM OLD.description OR
       NEW.unit IS DISTINCT FROM OLD.unit OR
       NEW.availability_rules IS DISTINCT FROM OLD.availability_rules THEN
        RAISE EXCEPTION 'inventory_catalog_offering_versions: pinned content (offering_id, version_number, description, unit, availability_rules) is immutable once written';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_catalog_version_content_mutation
    BEFORE UPDATE ON inventory_catalog_offering_versions
    FOR EACH ROW EXECUTE FUNCTION reject_catalog_version_content_mutation();

-- CatalogVariant — the spec's own "variants" input, one row per variant
-- declared at CreateOffering/CreateVersion time. Append-only: a variant
-- set is part of its version's own pinned content.
CREATE TABLE inventory_catalog_variants (
    variant_id   UUID PRIMARY KEY,
    tenant_id    VARCHAR(255) NOT NULL,
    version_id   UUID NOT NULL REFERENCES inventory_catalog_offering_versions(version_id),
    variant_code VARCHAR(100) NOT NULL,
    variant_name VARCHAR(255) NOT NULL,

    UNIQUE (tenant_id, version_id, variant_code)
);

CREATE OR REPLACE FUNCTION reject_catalog_variant_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'inventory_catalog_variants is append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_catalog_variant_mutation
    BEFORE UPDATE OR DELETE ON inventory_catalog_variants
    FOR EACH ROW EXECUTE FUNCTION reject_catalog_variant_mutation();

-- CatalogMapping — LinkMapping's own authority, "linked tax/accounting/
-- product mappings." Scoped to a version (not the offering) for the same
-- pinning reason as everything else here: a mapping in force when a
-- transaction referenced a version must never silently change underneath
-- that transaction. Append-only — a mapping is replaced by linking a new
-- one on a NEW version, never edited on an existing version.
CREATE TABLE inventory_catalog_mappings (
    mapping_id           UUID PRIMARY KEY,
    tenant_id            VARCHAR(255) NOT NULL,
    version_id           UUID NOT NULL REFERENCES inventory_catalog_offering_versions(version_id),
    mapping_type         VARCHAR(30) NOT NULL, -- TAX|ACCOUNTING|PRODUCT
    mapping_ref          VARCHAR(255) NOT NULL,
    linked_at            TIMESTAMP WITH TIME ZONE NOT NULL,
    linked_by_principal_id VARCHAR(255) NOT NULL,

    CONSTRAINT chk_catalog_mapping_type CHECK (mapping_type IN ('TAX', 'ACCOUNTING', 'PRODUCT')),
    UNIQUE (tenant_id, version_id, mapping_type)
);

CREATE OR REPLACE FUNCTION reject_catalog_mapping_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'inventory_catalog_mappings is append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_catalog_mapping_mutation
    BEFORE UPDATE OR DELETE ON inventory_catalog_mappings
    FOR EACH ROW EXECUTE FUNCTION reject_catalog_mapping_mutation();

ALTER TABLE inventory_catalog_offerings ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_catalog_offerings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_catalog_offerings
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE inventory_catalog_offering_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_catalog_offering_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_catalog_offering_versions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE inventory_catalog_variants ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_catalog_variants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_catalog_variants
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE inventory_catalog_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_catalog_mappings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_catalog_mappings
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_catalog_offerings_entity ON inventory_catalog_offerings (tenant_id, legal_entity_id);
CREATE INDEX idx_catalog_offerings_category ON inventory_catalog_offerings (tenant_id, legal_entity_id, category);
CREATE INDEX idx_catalog_versions_offering ON inventory_catalog_offering_versions (tenant_id, offering_id);
CREATE INDEX idx_catalog_variants_version ON inventory_catalog_variants (tenant_id, version_id);
CREATE INDEX idx_catalog_mappings_version ON inventory_catalog_mappings (tenant_id, version_id);
