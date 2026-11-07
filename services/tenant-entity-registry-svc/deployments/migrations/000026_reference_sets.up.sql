-- 000026_reference_sets.up.sql
--
-- MDM-04 Reference Release (ZS-SVC-U-001 s15-16): governed, domain-neutral code lists used by several services, held as
-- versioned effective-dated releases. A release is a set of changes (ADD / MODIFY_METADATA / SUPERSEDE / RETIRE / CORRECT) to
-- the codes of one set, with its source authority, publication timestamp kept apart from its business effective date, a
-- compatibility statement and an evidence reference. A code is read as of a date, or under one exact release, from the
-- published releases only. Releasing is maker-checker: whoever staged a release does not publish it. A published release never
-- changes; a later release supersedes it. Official and custom (user-assigned) codes are distinguished on every entry.
-- Nothing is deleted.

CREATE TABLE reference_sets (
    set_id           UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL REFERENCES tenants(tenant_id),
    set_code         VARCHAR(40) NOT NULL,
    name             TEXT NOT NULL,
    description      TEXT,
    source_authority TEXT NOT NULL,
    -- Whether a release may add user-assigned codes to the set; an official code list takes none.
    allow_custom     BOOLEAN NOT NULL DEFAULT FALSE,
    created_by       VARCHAR(255) NOT NULL,
    created_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id   VARCHAR(255),
    CONSTRAINT rs_tenant_unique UNIQUE (tenant_id, set_id),
    CONSTRAINT rs_code_unique UNIQUE (tenant_id, set_code),
    CONSTRAINT rs_code_shape CHECK (set_code ~ '^[A-Z][A-Z0-9_]{1,39}$'),
    CONSTRAINT rs_present CHECK (btrim(name) <> '' AND btrim(source_authority) <> '')
);

CREATE TABLE reference_releases (
    release_id          UUID PRIMARY KEY,
    tenant_id           UUID NOT NULL,
    set_id              UUID NOT NULL,
    release_version     VARCHAR(40) NOT NULL,
    status              VARCHAR(10) NOT NULL,
    -- When the source published it, apart from the business date from which it governs.
    source_published_at TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_from      DATE NOT NULL,
    source_authority    TEXT NOT NULL,
    compatibility       VARCHAR(20) NOT NULL,
    checksum            VARCHAR(64),
    evidence_ref        TEXT NOT NULL,
    staged_by           VARCHAR(255) NOT NULL,
    staged_at           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    released_by         VARCHAR(255),
    released_at         TIMESTAMP WITH TIME ZONE,
    release_evidence    TEXT,
    -- 1, 2, 3 ... in the order the set's releases took effect.
    publish_seq         INTEGER,
    correlation_id      VARCHAR(255),
    CONSTRAINT rr_set_in_tenant FOREIGN KEY (tenant_id, set_id) REFERENCES reference_sets (tenant_id, set_id),
    CONSTRAINT rr_tenant_unique UNIQUE (tenant_id, release_id),
    CONSTRAINT rr_version_unique UNIQUE (set_id, release_version),
    CONSTRAINT rr_version_shape CHECK (release_version ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$'),
    CONSTRAINT rr_status_known CHECK (status IN ('STAGED', 'PUBLISHED')),
    CONSTRAINT rr_compat_known CHECK (compatibility IN ('AUTO_ADOPT', 'REQUIRES_MIGRATION', 'PRESERVE_OLD')),
    CONSTRAINT rr_checksum_shape CHECK (checksum IS NULL OR checksum ~ '^[0-9a-f]{64}$'),
    CONSTRAINT rr_present CHECK (btrim(source_authority) <> '' AND btrim(evidence_ref) <> ''),
    CONSTRAINT rr_published_consistent CHECK ((status = 'PUBLISHED') = (released_by IS NOT NULL AND released_at IS NOT NULL
        AND publish_seq IS NOT NULL AND btrim(COALESCE(release_evidence, '')) <> '')),
    -- The release gate: the principal who staged a release is not the one who publishes it.
    CONSTRAINT rr_staged_clean CHECK (status = 'PUBLISHED' OR (released_by IS NULL AND released_at IS NULL AND publish_seq IS NULL)),
    CONSTRAINT rr_independent CHECK (released_by IS NULL OR released_by <> staged_by)
);
CREATE UNIQUE INDEX rr_seq_unique ON reference_releases (set_id, publish_seq) WHERE publish_seq IS NOT NULL;
CREATE INDEX idx_rr_set ON reference_releases (tenant_id, set_id, status, effective_from);

CREATE TABLE reference_entries (
    entry_id         UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL,
    set_id           UUID NOT NULL,
    release_id       UUID NOT NULL,
    code             VARCHAR(64) NOT NULL,
    change_type      VARCHAR(16) NOT NULL,
    label            TEXT,
    origin           VARCHAR(8) NOT NULL,
    metadata         JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- For SUPERSEDE: the code that takes over.
    replaced_by_code VARCHAR(64),
    reason           TEXT,
    CONSTRAINT re_release_in_tenant FOREIGN KEY (tenant_id, release_id) REFERENCES reference_releases (tenant_id, release_id),
    CONSTRAINT re_one_per_release UNIQUE (release_id, code),
    CONSTRAINT re_code_shape CHECK (code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    CONSTRAINT re_change_known CHECK (change_type IN ('ADD', 'MODIFY_METADATA', 'SUPERSEDE', 'RETIRE', 'CORRECT')),
    CONSTRAINT re_origin_known CHECK (origin IN ('OFFICIAL', 'CUSTOM')),
    CONSTRAINT re_label_when_live CHECK (change_type IN ('RETIRE', 'SUPERSEDE') OR btrim(COALESCE(label, '')) <> ''),
    CONSTRAINT re_replacement CHECK ((change_type = 'SUPERSEDE') = (replaced_by_code IS NOT NULL)),
    CONSTRAINT re_not_self_replaced CHECK (replaced_by_code IS NULL OR replaced_by_code <> code),
    CONSTRAINT re_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);
CREATE INDEX idx_re_code ON reference_entries (tenant_id, set_id, code);

-- A release changes only by being published, once, and a published release is final.
CREATE OR REPLACE FUNCTION reference_release_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reference releases are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'PUBLISHED' THEN
        RAISE EXCEPTION 'a published release is final; supersede it with a later release' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.set_id <> OLD.set_id OR NEW.release_version <> OLD.release_version
       OR NEW.effective_from <> OLD.effective_from OR NEW.source_published_at <> OLD.source_published_at
       OR NEW.compatibility <> OLD.compatibility OR NEW.staged_by <> OLD.staged_by OR NEW.evidence_ref <> OLD.evidence_ref THEN
        RAISE EXCEPTION 'a staged release changes only by being published' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- A release takes effect after the one before it, so published history is never rewritten.
    IF NEW.status = 'PUBLISHED' AND EXISTS (
        SELECT 1 FROM reference_releases o WHERE o.set_id = NEW.set_id AND o.status = 'PUBLISHED' AND o.effective_from >= NEW.effective_from) THEN
        RAISE EXCEPTION 'a release takes effect after every release already published' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER reference_release_guard BEFORE UPDATE OR DELETE ON reference_releases
    FOR EACH ROW EXECUTE FUNCTION reference_release_guard();

-- Entries are written once, into a staged release.
CREATE OR REPLACE FUNCTION reference_entry_guard() RETURNS trigger AS $$
DECLARE r reference_releases%ROWTYPE;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'reference entries are immutable and never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT * INTO r FROM reference_releases WHERE tenant_id = NEW.tenant_id AND release_id = NEW.release_id;
    IF r.status <> 'STAGED' OR r.set_id <> NEW.set_id THEN
        RAISE EXCEPTION 'entries can only be added to a staged release of the same set' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.origin = 'CUSTOM' AND NOT (SELECT allow_custom FROM reference_sets WHERE tenant_id = NEW.tenant_id AND set_id = NEW.set_id) THEN
        RAISE EXCEPTION 'this reference set takes no user-assigned codes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER reference_entry_guard BEFORE INSERT OR UPDATE OR DELETE ON reference_entries
    FOR EACH ROW EXECUTE FUNCTION reference_entry_guard();

CREATE OR REPLACE FUNCTION reference_set_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'a reference set is immutable and never deleted' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER reference_set_guard BEFORE UPDATE OR DELETE ON reference_sets
    FOR EACH ROW EXECUTE FUNCTION reference_set_guard();

ALTER TABLE reference_sets ENABLE ROW LEVEL SECURITY;
ALTER TABLE reference_sets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON reference_sets FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE reference_releases ENABLE ROW LEVEL SECURITY;
ALTER TABLE reference_releases FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON reference_releases FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE reference_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE reference_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON reference_entries FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
