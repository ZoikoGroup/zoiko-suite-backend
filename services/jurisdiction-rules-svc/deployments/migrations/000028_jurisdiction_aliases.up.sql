-- 000028_jurisdiction_aliases.up.sql
--
-- REF-01 JurisdictionAlias (ZS-SVC-I-001 s4.11): "internal jurisdiction UUID
-- stable across aliases", "ISO source update versioned", "Unknown/retired code
-- explicit", "never guess from free text".
--
-- A jurisdiction has one stable internal id and any number of CODES that other
-- systems and standards know it by: ISO 3166-1 alpha-2/alpha-3/numeric, ISO
-- 3166-2, UN M49, or an external system's own code. An alias maps one
-- (system, code) to one jurisdiction for an interval, with the standard and
-- version it came from and why the mapping is right.
--
-- Effective-dated, not edited: changing what a code means ends the old mapping
-- and adds a new one, so "what did code X mean on date D" always has an exact
-- answer. A (system, code) can mean only one jurisdiction at any instant, and
-- the DATABASE enforces it with an exclusion constraint, so two concurrent
-- amendments cannot both succeed.

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE IF NOT EXISTS jurisdiction_aliases (
    alias_id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_id   UUID         NOT NULL REFERENCES jurisdictions (jurisdiction_id),

    alias_system      VARCHAR(64)  NOT NULL,
    alias_code        VARCHAR(64)  NOT NULL,
    -- Case and surrounding whitespace are not meaning: "gb" and " GB " are GB.
    -- Nothing else is normalised, and nothing is ever matched by similarity.
    alias_code_norm   VARCHAR(64)  GENERATED ALWAYS AS (upper(btrim(alias_code))) STORED,

    -- Evidence (s4.11 "Source standard/version, import hash, effective dates,
    -- mapping rationale").
    source_standard   VARCHAR(128) NOT NULL,
    source_version    VARCHAR(64)  NOT NULL,
    mapping_rationale TEXT         NOT NULL,
    evidence_ref      TEXT,

    valid_from        TIMESTAMPTZ  NOT NULL,
    valid_to          TIMESTAMPTZ,
    alias_period      TSTZRANGE    GENERATED ALWAYS AS (tstzrange(valid_from, valid_to, '[)')) STORED,

    created_by        TEXT         NOT NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT clock_timestamp(),

    -- Set when a mapping is ended after the fact, not when it was created bounded.
    ended_by          TEXT,
    ended_at          TIMESTAMPTZ,
    end_reason        TEXT,

    CONSTRAINT ck_alias_interval CHECK (valid_to IS NULL OR valid_to > valid_from),
    CONSTRAINT ck_alias_present  CHECK (btrim(alias_system) <> '' AND btrim(alias_code) <> ''
                                        AND btrim(source_standard) <> '' AND btrim(source_version) <> ''
                                        AND btrim(mapping_rationale) <> ''),
    CONSTRAINT ck_alias_end_complete CHECK (
        (ended_at IS NULL AND ended_by IS NULL AND end_reason IS NULL)
        OR (ended_at IS NOT NULL AND ended_by IS NOT NULL AND btrim(COALESCE(end_reason, '')) <> '')),
    -- A code cannot mean two jurisdictions at once.
    CONSTRAINT excl_alias_one_meaning_at_a_time
        EXCLUDE USING GIST (alias_system WITH =, alias_code_norm WITH =, alias_period WITH &&)
);

CREATE INDEX IF NOT EXISTS idx_alias_jurisdiction
    ON jurisdiction_aliases (jurisdiction_id, valid_from);

-- A mapping is evidence. The only change a row ever sees is being ended, once:
-- valid_to and the three ended_* columns going from empty to filled together.
CREATE OR REPLACE FUNCTION jurisdiction_aliases_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'jurisdiction_aliases is append-only (% refused)', TG_OP
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.ended_at IS NOT NULL
       OR NEW.jurisdiction_id  IS DISTINCT FROM OLD.jurisdiction_id
       OR NEW.alias_system     IS DISTINCT FROM OLD.alias_system
       OR NEW.alias_code       IS DISTINCT FROM OLD.alias_code
       OR NEW.source_standard  IS DISTINCT FROM OLD.source_standard
       OR NEW.source_version   IS DISTINCT FROM OLD.source_version
       OR NEW.mapping_rationale IS DISTINCT FROM OLD.mapping_rationale
       OR NEW.evidence_ref     IS DISTINCT FROM OLD.evidence_ref
       OR NEW.valid_from       IS DISTINCT FROM OLD.valid_from
       OR NEW.created_by       IS DISTINCT FROM OLD.created_by
       OR NEW.created_at       IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'jurisdiction_aliases is append-only: a mapping can only be ended, once'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_jurisdiction_aliases_guard ON jurisdiction_aliases;
CREATE TRIGGER trg_jurisdiction_aliases_guard
    BEFORE UPDATE OR DELETE ON jurisdiction_aliases
    FOR EACH ROW EXECUTE FUNCTION jurisdiction_aliases_guard();

DROP TRIGGER IF EXISTS trg_jurisdiction_aliases_no_truncate ON jurisdiction_aliases;
CREATE TRIGGER trg_jurisdiction_aliases_no_truncate
    BEFORE TRUNCATE ON jurisdiction_aliases
    FOR EACH STATEMENT EXECUTE FUNCTION jurisdiction_aliases_guard();

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT, UPDATE ON jurisdiction_aliases TO app_jurisdiction_rules;
    END IF;
END
$$;
