-- 000005_jurisdiction_pack_registries.up.sql
-- ZS-JUR-001 Wave 0: regime, source, interpretation and pack registries.
--
-- Additive only. Nothing in 000001-000004 is altered except nullable
-- provenance columns on jurisdiction_rules, so every existing caller and
-- query keeps working unchanged.
--
-- Like the rest of this service these are platform-wide reference data: no
-- tenant_id, no RLS (ZS-JUR-001 s3: packs are not tenant data).
-- Status and type fields are VARCHAR (service doctrine: values are data).
-- Lifecycle INTEGRITY, though, is enforced here by triggers, because the
-- document's invariants (released = immutable, sources are evidence, a
-- reviewer is independent of the author) must hold even for a writer that
-- bypasses the Go code.

-- ── regimes (s4) ────────────────────────────────────────────────────────────
CREATE TABLE regulatory_regimes (
    regime_id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    regime_code             VARCHAR(64)  NOT NULL,
    regime_name             TEXT         NOT NULL,
    description             TEXT,
    active_flag             BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL,
    schema_version          VARCHAR(16)  NOT NULL DEFAULT '1.0',
    CONSTRAINT uq_regulatory_regimes_code UNIQUE (regime_code)
);

-- ── source register (s8) ────────────────────────────────────────────────────
CREATE TABLE regulatory_sources (
    source_id                UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_id          UUID         NOT NULL REFERENCES jurisdictions(jurisdiction_id),
    authority                TEXT         NOT NULL,
    source_type              VARCHAR(64)  NOT NULL,
    authority_level          VARCHAR(64)  NOT NULL,
    title                    TEXT         NOT NULL,
    official_identifier      TEXT,
    published_on             DATE,
    effective_on             DATE,
    location                 TEXT         NOT NULL,
    -- Evidence that the reviewed source can be identified later (s8).
    snapshot_hash            VARCHAR(71)  NOT NULL,
    snapshot_ref             TEXT,
    language                 VARCHAR(16)  NOT NULL DEFAULT 'en',
    translation_ref          TEXT,
    interpretation_notes     TEXT,
    -- Independent review: the reviewer may not be the author.
    reviewed_by_principal_id TEXT,
    reviewed_at              TIMESTAMPTZ,
    superseded_by_source_id  UUID         REFERENCES regulatory_sources(source_id),
    superseded_at            TIMESTAMPTZ,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT         NOT NULL,
    schema_version           VARCHAR(16)  NOT NULL DEFAULT '1.0',
    CONSTRAINT ck_source_snapshot_hash CHECK (snapshot_hash ~ '^sha256:[0-9a-f]{64}$'),
    CONSTRAINT ck_source_reviewer_independent
        CHECK (reviewed_by_principal_id IS NULL OR reviewed_by_principal_id <> created_by_principal_id),
    CONSTRAINT ck_source_review_pair
        CHECK ((reviewed_by_principal_id IS NULL) = (reviewed_at IS NULL)),
    CONSTRAINT ck_source_not_self_superseded
        CHECK (superseded_by_source_id IS NULL OR superseded_by_source_id <> source_id)
);

-- Same authority + same captured bytes + same jurisdiction = same source.
CREATE UNIQUE INDEX uq_regulatory_sources_snapshot
    ON regulatory_sources (jurisdiction_id, authority, snapshot_hash);
CREATE INDEX idx_regulatory_sources_jurisdiction ON regulatory_sources (jurisdiction_id);

-- ── interpretation records (s8, s20) ────────────────────────────────────────
CREATE TABLE interpretation_records (
    interpretation_id        UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_id          UUID         NOT NULL REFERENCES jurisdictions(jurisdiction_id),
    regime_id                UUID         REFERENCES regulatory_regimes(regime_id),
    subject                  TEXT         NOT NULL,
    decision                 TEXT         NOT NULL,
    rationale                TEXT         NOT NULL,
    -- PENDING -> APPROVED. An interpretation is never authoritative until a
    -- qualified human who did not write it approves it (s3, JUR-NEG-18).
    status                   VARCHAR(32)  NOT NULL DEFAULT 'PENDING',
    approved_by_principal_id TEXT,
    approved_at              TIMESTAMPTZ,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT         NOT NULL,
    schema_version           VARCHAR(16)  NOT NULL DEFAULT '1.0',
    CONSTRAINT ck_interpretation_approver_independent
        CHECK (approved_by_principal_id IS NULL OR approved_by_principal_id <> created_by_principal_id),
    CONSTRAINT ck_interpretation_approval_consistent
        CHECK ((status = 'APPROVED') = (approved_by_principal_id IS NOT NULL AND approved_at IS NOT NULL))
);
CREATE INDEX idx_interpretation_jurisdiction ON interpretation_records (jurisdiction_id);

CREATE TABLE interpretation_sources (
    interpretation_id UUID NOT NULL REFERENCES interpretation_records(interpretation_id),
    source_id         UUID NOT NULL REFERENCES regulatory_sources(source_id),
    PRIMARY KEY (interpretation_id, source_id)
);

-- ── packs and pack versions (s5, s6, s27) ───────────────────────────────────
CREATE TABLE jurisdiction_packs (
    pack_id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    pack_ref                VARCHAR(128) NOT NULL,
    pack_name               TEXT         NOT NULL,
    owner                   TEXT         NOT NULL,
    support_owner           TEXT,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL,
    schema_version          VARCHAR(16)  NOT NULL DEFAULT '1.0',
    CONSTRAINT uq_jurisdiction_packs_ref UNIQUE (pack_ref),
    CONSTRAINT ck_pack_ref_shape CHECK (pack_ref ~ '^[a-z][a-z0-9]*(\.[a-z0-9-]+)+$')
);

CREATE TABLE jurisdiction_pack_versions (
    pack_version_id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    pack_id                 UUID         NOT NULL REFERENCES jurisdiction_packs(pack_id),
    version                 VARCHAR(32)  NOT NULL,
    -- Draft, Review, Certified, Released, Withdrawn, Superseded,
    -- Emergency blocked (s23). Edges are enforced by trigger below.
    status                  VARCHAR(32)  NOT NULL DEFAULT 'DRAFT',
    effective_from          DATE         NOT NULL,
    effective_to            DATE,
    manifest                JSONB        NOT NULL,
    -- sha256 of the canonical manifest, computed by the server. Wave 1 adds
    -- the artifact digest, test bundle digest and signature.
    manifest_digest         VARCHAR(71)  NOT NULL,
    artifact_digest         VARCHAR(71),
    test_bundle_digest      VARCHAR(71),
    signature               TEXT,
    signature_key_ref       TEXT,
    source_register_version INTEGER,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL,
    updated_at              TIMESTAMPTZ,
    updated_by_principal_id TEXT,
    schema_version          VARCHAR(16)  NOT NULL DEFAULT '1.0',
    CONSTRAINT uq_pack_version UNIQUE (pack_id, version),
    CONSTRAINT ck_pack_version_period CHECK (effective_to IS NULL OR effective_to > effective_from),
    CONSTRAINT ck_pack_manifest_digest CHECK (manifest_digest ~ '^sha256:[0-9a-f]{64}$'),
    CONSTRAINT ck_pack_artifact_digest CHECK (artifact_digest IS NULL OR artifact_digest ~ '^sha256:[0-9a-f]{64}$'),
    CONSTRAINT ck_pack_test_digest CHECK (test_bundle_digest IS NULL OR test_bundle_digest ~ '^sha256:[0-9a-f]{64}$')
);
CREATE INDEX idx_pack_versions_pack ON jurisdiction_pack_versions (pack_id, created_at DESC);

CREATE TABLE pack_version_jurisdictions (
    pack_version_id UUID NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    jurisdiction_id UUID NOT NULL REFERENCES jurisdictions(jurisdiction_id),
    PRIMARY KEY (pack_version_id, jurisdiction_id)
);
CREATE TABLE pack_version_regimes (
    pack_version_id UUID NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    regime_id       UUID NOT NULL REFERENCES regulatory_regimes(regime_id),
    PRIMARY KEY (pack_version_id, regime_id)
);
-- Exact dependency graph (s27 PackDependency). dependency_ref is a pack_ref
-- or an external code-list / schema reference (e.g. ref.iso4217), pinned to
-- an exact version.
CREATE TABLE pack_dependencies (
    pack_version_id    UUID         NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    dependency_ref     VARCHAR(128) NOT NULL,
    dependency_version VARCHAR(64)  NOT NULL,
    PRIMARY KEY (pack_version_id, dependency_ref)
);

-- ── rule provenance (s7): nullable, additive ────────────────────────────────
ALTER TABLE jurisdiction_rules
    ADD COLUMN regime_id           UUID REFERENCES regulatory_regimes(regime_id),
    ADD COLUMN interpretation_id   UUID REFERENCES interpretation_records(interpretation_id),
    ADD COLUMN supersedes_rule_id  UUID REFERENCES jurisdiction_rules(jurisdiction_rule_id),
    -- Declared precedence; never implicit (s7, s10). Higher wins.
    ADD COLUMN precedence          INTEGER,
    ADD COLUMN published_on        DATE;

CREATE TABLE rule_sources (
    jurisdiction_rule_id UUID NOT NULL REFERENCES jurisdiction_rules(jurisdiction_rule_id),
    source_id            UUID NOT NULL REFERENCES regulatory_sources(source_id),
    PRIMARY KEY (jurisdiction_rule_id, source_id)
);

-- ── integrity triggers ──────────────────────────────────────────────────────

-- Evidence is never deleted.
CREATE FUNCTION jur_forbid_delete() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: rows are never deleted (ZS-JUR-001)', TG_TABLE_NAME
        USING ERRCODE = '23514';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_regulatory_sources_no_delete BEFORE DELETE ON regulatory_sources
    FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
CREATE TRIGGER trg_interpretation_records_no_delete BEFORE DELETE ON interpretation_records
    FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
CREATE TRIGGER trg_pack_versions_no_delete BEFORE DELETE ON jurisdiction_pack_versions
    FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
CREATE TRIGGER trg_interpretation_sources_no_delete BEFORE DELETE ON interpretation_sources
    FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();

-- A captured source is evidence: only the independent review and the
-- supersede link may be added, each exactly once; nothing else changes.
CREATE FUNCTION jur_source_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.source_id <> OLD.source_id
       OR NEW.jurisdiction_id <> OLD.jurisdiction_id
       OR NEW.authority <> OLD.authority
       OR NEW.source_type <> OLD.source_type
       OR NEW.authority_level <> OLD.authority_level
       OR NEW.title <> OLD.title
       OR NEW.official_identifier IS DISTINCT FROM OLD.official_identifier
       OR NEW.published_on IS DISTINCT FROM OLD.published_on
       OR NEW.effective_on IS DISTINCT FROM OLD.effective_on
       OR NEW.location <> OLD.location
       OR NEW.snapshot_hash <> OLD.snapshot_hash
       OR NEW.snapshot_ref IS DISTINCT FROM OLD.snapshot_ref
       OR NEW.language <> OLD.language
       OR NEW.translation_ref IS DISTINCT FROM OLD.translation_ref
       OR NEW.interpretation_notes IS DISTINCT FROM OLD.interpretation_notes
       OR NEW.created_at <> OLD.created_at
       OR NEW.created_by_principal_id <> OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'regulatory source % is immutable evidence', OLD.source_id USING ERRCODE = '23514';
    END IF;
    IF OLD.reviewed_by_principal_id IS NOT NULL
       AND (NEW.reviewed_by_principal_id IS DISTINCT FROM OLD.reviewed_by_principal_id
            OR NEW.reviewed_at IS DISTINCT FROM OLD.reviewed_at) THEN
        RAISE EXCEPTION 'regulatory source % review is already recorded', OLD.source_id USING ERRCODE = '23514';
    END IF;
    IF OLD.superseded_by_source_id IS NOT NULL
       AND (NEW.superseded_by_source_id IS DISTINCT FROM OLD.superseded_by_source_id
            OR NEW.superseded_at IS DISTINCT FROM OLD.superseded_at) THEN
        RAISE EXCEPTION 'regulatory source % is already superseded', OLD.source_id USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_regulatory_sources_guard BEFORE UPDATE ON regulatory_sources
    FOR EACH ROW EXECUTE FUNCTION jur_source_guard();

-- An interpretation moves PENDING -> APPROVED once; after that it is frozen.
CREATE FUNCTION jur_interpretation_guard() RETURNS trigger AS $$
BEGIN
    IF OLD.status = 'APPROVED' THEN
        RAISE EXCEPTION 'interpretation % is approved and immutable', OLD.interpretation_id USING ERRCODE = '23514';
    END IF;
    IF NEW.interpretation_id <> OLD.interpretation_id
       OR NEW.jurisdiction_id <> OLD.jurisdiction_id
       OR NEW.regime_id IS DISTINCT FROM OLD.regime_id
       OR NEW.subject <> OLD.subject
       OR NEW.decision <> OLD.decision
       OR NEW.rationale <> OLD.rationale
       OR NEW.created_by_principal_id <> OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'interpretation % content cannot change; create a new record', OLD.interpretation_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_interpretation_guard BEFORE UPDATE ON interpretation_records
    FOR EACH ROW EXECUTE FUNCTION jur_interpretation_guard();

-- Source links of an interpretation are fixed once it is approved.
CREATE FUNCTION jur_interpretation_sources_guard() RETURNS trigger AS $$
BEGIN
    IF (SELECT status FROM interpretation_records WHERE interpretation_id = NEW.interpretation_id) = 'APPROVED' THEN
        RAISE EXCEPTION 'interpretation % is approved; its sources are fixed', NEW.interpretation_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_interpretation_sources_guard BEFORE INSERT ON interpretation_sources
    FOR EACH ROW EXECUTE FUNCTION jur_interpretation_sources_guard();

-- Pack versions: legal status edges (s23); manifest, identity, dates and
-- digests are frozen as soon as the version leaves DRAFT, and an artifact
-- digest or signature, once set, never changes (JUR-NEG-20).
CREATE FUNCTION jur_pack_version_guard() RETURNS trigger AS $$
DECLARE
    ok boolean;
BEGIN
    IF NEW.pack_version_id <> OLD.pack_version_id OR NEW.pack_id <> OLD.pack_id OR NEW.version <> OLD.version
       OR NEW.created_at <> OLD.created_at OR NEW.created_by_principal_id <> OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'pack version identity is immutable' USING ERRCODE = '23514';
    END IF;

    IF OLD.status <> 'DRAFT' AND (
           NEW.manifest::text <> OLD.manifest::text
        OR NEW.manifest_digest <> OLD.manifest_digest
        OR NEW.effective_from <> OLD.effective_from
        OR NEW.effective_to IS DISTINCT FROM OLD.effective_to) THEN
        RAISE EXCEPTION 'pack version % is past DRAFT; its content is immutable, create a new version', OLD.version
            USING ERRCODE = '23514';
    END IF;

    IF (OLD.artifact_digest IS NOT NULL AND NEW.artifact_digest IS DISTINCT FROM OLD.artifact_digest)
       OR (OLD.test_bundle_digest IS NOT NULL AND NEW.test_bundle_digest IS DISTINCT FROM OLD.test_bundle_digest)
       OR (OLD.signature IS NOT NULL AND NEW.signature IS DISTINCT FROM OLD.signature) THEN
        RAISE EXCEPTION 'pack version % digests and signature are write-once', OLD.version USING ERRCODE = '23514';
    END IF;

    IF NEW.status <> OLD.status THEN
        ok := (OLD.status, NEW.status) IN (
            ('DRAFT', 'REVIEW'),
            ('REVIEW', 'DRAFT'),
            ('REVIEW', 'CERTIFIED'),
            ('CERTIFIED', 'RELEASED'),
            ('CERTIFIED', 'WITHDRAWN'),
            ('RELEASED', 'WITHDRAWN'),
            ('RELEASED', 'SUPERSEDED'),
            ('RELEASED', 'EMERGENCY_BLOCKED'),
            ('EMERGENCY_BLOCKED', 'RELEASED'),
            ('EMERGENCY_BLOCKED', 'WITHDRAWN'),
            ('SUPERSEDED', 'WITHDRAWN'));
        IF NOT ok THEN
            RAISE EXCEPTION 'illegal pack version status transition % -> %', OLD.status, NEW.status
                USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pack_version_guard BEFORE UPDATE ON jurisdiction_pack_versions
    FOR EACH ROW EXECUTE FUNCTION jur_pack_version_guard();

-- The scope and dependency rows of a version are fixed once it leaves DRAFT.
CREATE FUNCTION jur_pack_children_guard() RETURNS trigger AS $$
BEGIN
    IF (SELECT status FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id) <> 'DRAFT' THEN
        RAISE EXCEPTION 'pack version is past DRAFT; % cannot change', TG_TABLE_NAME USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pack_version_jurisdictions_guard BEFORE INSERT ON pack_version_jurisdictions
    FOR EACH ROW EXECUTE FUNCTION jur_pack_children_guard();
CREATE TRIGGER trg_pack_version_regimes_guard BEFORE INSERT ON pack_version_regimes
    FOR EACH ROW EXECUTE FUNCTION jur_pack_children_guard();
CREATE TRIGGER trg_pack_dependencies_guard BEFORE INSERT ON pack_dependencies
    FOR EACH ROW EXECUTE FUNCTION jur_pack_children_guard();

-- Rule provenance may only be edited while the rule is still a DRAFT (s3:
-- released rules are immutable; corrections create a new version).
CREATE FUNCTION jur_rule_provenance_guard() RETURNS trigger AS $$
BEGIN
    IF OLD.rule_status <> 'DRAFT' AND (
           NEW.regime_id IS DISTINCT FROM OLD.regime_id
        OR NEW.interpretation_id IS DISTINCT FROM OLD.interpretation_id
        OR NEW.supersedes_rule_id IS DISTINCT FROM OLD.supersedes_rule_id
        OR NEW.precedence IS DISTINCT FROM OLD.precedence
        OR NEW.published_on IS DISTINCT FROM OLD.published_on) THEN
        RAISE EXCEPTION 'rule % is past DRAFT; its provenance is immutable', OLD.jurisdiction_rule_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_jurisdiction_rules_provenance_guard BEFORE UPDATE ON jurisdiction_rules
    FOR EACH ROW EXECUTE FUNCTION jur_rule_provenance_guard();

CREATE FUNCTION jur_rule_sources_guard() RETURNS trigger AS $$
BEGIN
    IF (SELECT rule_status FROM jurisdiction_rules WHERE jurisdiction_rule_id = NEW.jurisdiction_rule_id) <> 'DRAFT' THEN
        RAISE EXCEPTION 'rule is past DRAFT; its sources cannot change' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_rule_sources_guard BEFORE INSERT ON rule_sources
    FOR EACH ROW EXECUTE FUNCTION jur_rule_sources_guard();
CREATE FUNCTION jur_rule_sources_delete_guard() RETURNS trigger AS $$
BEGIN
    IF (SELECT rule_status FROM jurisdiction_rules WHERE jurisdiction_rule_id = OLD.jurisdiction_rule_id) <> 'DRAFT' THEN
        RAISE EXCEPTION 'rule is past DRAFT; its sources cannot change' USING ERRCODE = '23514';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_rule_sources_delete_guard BEFORE DELETE ON rule_sources
    FOR EACH ROW EXECUTE FUNCTION jur_rule_sources_delete_guard();
