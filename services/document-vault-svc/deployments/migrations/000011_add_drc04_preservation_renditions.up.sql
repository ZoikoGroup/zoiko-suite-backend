-- DRC-04 Preservation, Rendition & Content Integrity (ZS-SVC-S-001 §6).
-- Additive alongside documents/document_versions (000001) and
-- records (000009), which are untouched.
--
-- DRC-I15: original and derivatives are distinct objects with separate
-- hashes — a rendition is always a new row, never a rewrite of
-- document_versions. DRC-I16: OCR text is non-authoritative content,
-- not a record, unless separately declared one (DRC-02 handles that
-- declaration; this migration does not special-case OCR_TEXT beyond
-- naming it a valid rendition_class). DRC-I17: redaction always creates
-- a derivative rendition — the unredacted source is never overwritten
-- or deleted. DRC-I20: every preservation/access/redacted rendition
-- gets a fixity_manifest. DRC-I27: an export package records exactly
-- which versions/renditions/redactions were included and which were
-- deliberately omitted, plus its own hash.

-- Renditions: derivative content objects. The ORIGINAL itself is
-- document_versions — this table only holds things derived FROM an
-- original (or from another rendition, via parent_rendition_id, e.g. a
-- REDACTED_RENDITION built from an ACCESS_RENDITION). Append-only: a
-- failed or superseded transformation is never fixed in place, it is a
-- new row (or a fixity incident on the old one) — see
-- fixity_manifests below.
CREATE TABLE renditions (
    rendition_id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                  UUID NOT NULL,
    source_document_version_id UUID NOT NULL REFERENCES document_versions(document_version_id),
    parent_rendition_id        UUID REFERENCES renditions(rendition_id),
    rendition_class            VARCHAR(24) NOT NULL
        CHECK (rendition_class IN (
            'PRESERVATION_RENDITION', 'ACCESS_RENDITION', 'REDACTED_RENDITION', 'OCR_TEXT', 'NORMALIZED_DATA'
        )),
    transformation_profile     VARCHAR(100) NOT NULL CHECK (transformation_profile <> ''),
    checksum_sha256            VARCHAR(64) NOT NULL CHECK (length(checksum_sha256) = 64),
    storage_key                VARCHAR(500) NOT NULL CHECK (storage_key <> ''),
    size_bytes                 BIGINT NOT NULL CHECK (size_bytes >= 0),
    content_type                VARCHAR(255) NOT NULL CHECK (content_type <> ''),
    created_by_principal_id    VARCHAR(255) NOT NULL CHECK (created_by_principal_id <> ''),
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_renditions_source_version ON renditions (source_document_version_id, rendition_class);
CREATE INDEX idx_renditions_tenant ON renditions (tenant_id, created_at DESC);

ALTER TABLE renditions ENABLE ROW LEVEL SECURITY;
ALTER TABLE renditions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON renditions
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

-- Append-only, no exceptions — a rendition is immutable from the
-- moment it is created (DRC-I15).
CREATE OR REPLACE FUNCTION reject_rendition_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'renditions rows are immutable and never deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_rendition_mutation
    BEFORE UPDATE OR DELETE ON renditions
    FOR EACH ROW EXECUTE FUNCTION reject_rendition_mutation();

-- Fixity Manifests: the integrity control for exactly one content
-- object — either an original document_version or a rendition, never
-- both (the XOR check below). integrity_state starts VERIFIED at
-- creation (the hash was just computed from the bytes that were
-- written) and only ever changes through a verification or repair
-- command — never backfilled or guessed.
--
-- State graph (DRC-I20, "every preservation copy has a verifiable
-- fixity manifest"; repair doctrine from §6's own caution that repair
-- "must restore correct bytes from a verified replica, never rewrite
-- metadata to fake a hash match"):
--   VERIFIED   -> VERIFIED | MISMATCH | MISSING | UNREADABLE   (routine re-verification)
--   REPAIRING  -> VERIFIED | MISMATCH | MISSING | UNREADABLE   (repair attempt re-verified)
--   MISMATCH/MISSING/UNREADABLE -> REPAIRING                   (repair started)
-- A failure state can only be exited via REPAIRING — never a bare
-- re-verification skip, so a known-bad manifest is always visibly
-- "being worked on" before it can claim VERIFIED again.
CREATE TABLE fixity_manifests (
    manifest_id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    document_version_id       UUID REFERENCES document_versions(document_version_id),
    rendition_id               UUID REFERENCES renditions(rendition_id),
    source_hash_algorithm     VARCHAR(16) NOT NULL DEFAULT 'SHA256',
    source_hash               VARCHAR(64) NOT NULL CHECK (length(source_hash) = 64),
    integrity_state           VARCHAR(16) NOT NULL DEFAULT 'VERIFIED'
        CHECK (integrity_state IN ('VERIFIED', 'MISMATCH', 'MISSING', 'UNREADABLE', 'REPAIRING')),
    generated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    generated_by_principal_id VARCHAR(255) NOT NULL CHECK (generated_by_principal_id <> ''),
    last_verified_at          TIMESTAMPTZ,
    last_verified_hash        VARCHAR(64),
    repair_source_ref         TEXT,
    repair_started_at         TIMESTAMPTZ,
    repaired_by_principal_id  VARCHAR(255),
    CHECK ((document_version_id IS NULL) <> (rendition_id IS NULL)),
    CHECK ((repair_source_ref IS NULL) = (repair_started_at IS NULL))
);

CREATE UNIQUE INDEX idx_fixity_manifests_one_per_version ON fixity_manifests (document_version_id) WHERE document_version_id IS NOT NULL;
CREATE UNIQUE INDEX idx_fixity_manifests_one_per_rendition ON fixity_manifests (rendition_id) WHERE rendition_id IS NOT NULL;
CREATE INDEX idx_fixity_manifests_tenant_state ON fixity_manifests (tenant_id, integrity_state);

ALTER TABLE fixity_manifests ENABLE ROW LEVEL SECURITY;
ALTER TABLE fixity_manifests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON fixity_manifests
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION reject_fixity_manifest_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'fixity_manifests rows are never deleted';
    END IF;
    IF NEW.document_version_id IS DISTINCT FROM OLD.document_version_id
        OR NEW.rendition_id IS DISTINCT FROM OLD.rendition_id
        OR NEW.source_hash_algorithm IS DISTINCT FROM OLD.source_hash_algorithm
        OR NEW.source_hash IS DISTINCT FROM OLD.source_hash
        OR NEW.generated_at IS DISTINCT FROM OLD.generated_at
        OR NEW.generated_by_principal_id IS DISTINCT FROM OLD.generated_by_principal_id
    THEN
        RAISE EXCEPTION 'fixity_manifest % facts are immutable', OLD.manifest_id;
    END IF;
    IF OLD.integrity_state <> NEW.integrity_state THEN
        CASE OLD.integrity_state
            WHEN 'VERIFIED' THEN
                IF NEW.integrity_state NOT IN ('VERIFIED', 'MISMATCH', 'MISSING', 'UNREADABLE') THEN
                    RAISE EXCEPTION 'invalid fixity_manifest transition from VERIFIED to %', NEW.integrity_state;
                END IF;
            WHEN 'REPAIRING' THEN
                IF NEW.integrity_state NOT IN ('VERIFIED', 'MISMATCH', 'MISSING', 'UNREADABLE') THEN
                    RAISE EXCEPTION 'invalid fixity_manifest transition from REPAIRING to %', NEW.integrity_state;
                END IF;
            WHEN 'MISMATCH', 'MISSING', 'UNREADABLE' THEN
                IF NEW.integrity_state <> 'REPAIRING' THEN
                    RAISE EXCEPTION 'invalid fixity_manifest transition from % to %', OLD.integrity_state, NEW.integrity_state;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown fixity_manifest integrity_state %', OLD.integrity_state;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_fixity_manifest_mutation
    BEFORE UPDATE OR DELETE ON fixity_manifests
    FOR EACH ROW EXECUTE FUNCTION reject_fixity_manifest_mutation();

-- Redaction Profiles: one per REDACTED_RENDITION (enforced in the
-- store, same style as retention_rule_versions' record_class/
-- jurisdiction match against its bound record — not every
-- cross-table rule needs a DB-level CHECK). DRC-I17: the profile
-- records what was removed and under what authority; it never
-- touches the unredacted source.
CREATE TABLE redaction_profiles (
    redaction_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL,
    rendition_id             UUID NOT NULL UNIQUE REFERENCES renditions(rendition_id),
    purpose                  TEXT NOT NULL CHECK (purpose <> ''),
    recipient_class          VARCHAR(100) NOT NULL CHECK (recipient_class <> ''),
    fields_removed           TEXT[] NOT NULL DEFAULT '{}',
    legal_basis_ref          TEXT NOT NULL DEFAULT '',
    approved_by_principal_id VARCHAR(255) NOT NULL CHECK (approved_by_principal_id <> ''),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_redaction_profiles_tenant ON redaction_profiles (tenant_id);

ALTER TABLE redaction_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE redaction_profiles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON redaction_profiles
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION reject_redaction_profile_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'redaction_profiles rows are immutable and never deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_redaction_profile_mutation
    BEFORE UPDATE OR DELETE ON redaction_profiles
    FOR EACH ROW EXECUTE FUNCTION reject_redaction_profile_mutation();

-- Export Packages: a sealed, point-in-time manifest of exactly what
-- left the vault in one export (DRC-I27). package_hash is computed by
-- the store from the ordered item hashes below — never caller-
-- supplied, so it cannot be asserted independent of what the package
-- actually contains.
CREATE TABLE export_packages (
    package_id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL,
    requested_by_principal_id VARCHAR(255) NOT NULL CHECK (requested_by_principal_id <> ''),
    package_hash              VARCHAR(64) NOT NULL CHECK (length(package_hash) = 64),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_export_packages_tenant ON export_packages (tenant_id, created_at DESC);

ALTER TABLE export_packages ENABLE ROW LEVEL SECURITY;
ALTER TABLE export_packages FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON export_packages
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION reject_export_package_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'export_packages rows are immutable and never deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_export_package_mutation
    BEFORE UPDATE OR DELETE ON export_packages
    FOR EACH ROW EXECUTE FUNCTION reject_export_package_mutation();

-- Export Package Items: one row per version/rendition the package
-- either included or deliberately omitted. item_type pins which FK
-- applies; included_hash is NULL for an omitted item (nothing was
-- exported for it to hash).
CREATE TABLE export_package_items (
    item_id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL,
    package_id           UUID NOT NULL REFERENCES export_packages(package_id),
    item_type            VARCHAR(16) NOT NULL CHECK (item_type IN ('DOCUMENT_VERSION', 'RENDITION')),
    document_version_id  UUID REFERENCES document_versions(document_version_id),
    rendition_id         UUID REFERENCES renditions(rendition_id),
    redaction_id         UUID REFERENCES redaction_profiles(redaction_id),
    included             BOOLEAN NOT NULL DEFAULT true,
    included_hash        VARCHAR(64),
    omission_reason      TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (item_type = 'DOCUMENT_VERSION' AND document_version_id IS NOT NULL AND rendition_id IS NULL)
        OR (item_type = 'RENDITION' AND rendition_id IS NOT NULL AND document_version_id IS NULL)
    ),
    CHECK (included = (omission_reason IS NULL)),
    CHECK (included = (included_hash IS NOT NULL))
);

CREATE INDEX idx_export_package_items_package ON export_package_items (package_id);

ALTER TABLE export_package_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE export_package_items FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON export_package_items
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION reject_export_package_item_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'export_package_items rows are immutable and never deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_export_package_item_mutation
    BEFORE UPDATE OR DELETE ON export_package_items
    FOR EACH ROW EXECUTE FUNCTION reject_export_package_item_mutation();
