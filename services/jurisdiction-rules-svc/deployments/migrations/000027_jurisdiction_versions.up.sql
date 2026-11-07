-- 000027_jurisdiction_versions.up.sql
--
-- REF-01 Jurisdiction Registry (ZS-SVC-I-001 s4.11): "hierarchy as-of exact",
-- "retired jurisdiction readable as of the past".
--
-- The jurisdictions table holds the CURRENT state of each jurisdiction and is
-- updated in place, so a past date had no answer: a parent change or rename
-- erased what the hierarchy looked like before it. jurisdictions stays the
-- current-state projection every existing reader uses, untouched.
-- jurisdiction_versions is the effective-dated history beside it: one row per
-- version of the attributes that matter for a point-in-time read (name, type,
-- parent, authority), each with the interval it was in force.
--
-- A TRIGGER maintains it, not application code, so every write path -- the
-- existing create and deactivate, the import that follows, anything added
-- later -- records history without anyone having to remember to.

-- ── Retirement evidence (RetireJurisdiction) ────────────────────────────────
ALTER TABLE jurisdictions
    ADD COLUMN IF NOT EXISTS retirement_reason            TEXT,
    ADD COLUMN IF NOT EXISTS retirement_evidence_ref      TEXT,
    ADD COLUMN IF NOT EXISTS superseded_by_jurisdiction_id UUID REFERENCES jurisdictions (jurisdiction_id);

ALTER TABLE jurisdictions
    DROP CONSTRAINT IF EXISTS ck_jurisdiction_successor;
ALTER TABLE jurisdictions
    ADD CONSTRAINT ck_jurisdiction_successor CHECK (
        superseded_by_jurisdiction_id IS NULL
        OR (active_flag = FALSE AND superseded_by_jurisdiction_id <> jurisdiction_id));

-- ── The versions ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS jurisdiction_versions (
    version_id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_id        UUID         NOT NULL REFERENCES jurisdictions (jurisdiction_id),
    version_no             INTEGER      NOT NULL,

    jurisdiction_name      TEXT         NOT NULL,
    jurisdiction_type      VARCHAR(64)  NOT NULL,
    parent_jurisdiction_id UUID         REFERENCES jurisdictions (jurisdiction_id),
    authority_type         VARCHAR(64)  NOT NULL,

    -- Business time: [valid_from, valid_to). NULL valid_to = still in force.
    valid_from             TIMESTAMPTZ  NOT NULL,
    valid_to               TIMESTAMPTZ,

    change_type            VARCHAR(24)  NOT NULL,
    changed_by             TEXT         NOT NULL,
    reason                 TEXT,
    evidence_ref           TEXT,
    -- Record time: when this service wrote the version.
    recorded_at            TIMESTAMPTZ  NOT NULL DEFAULT clock_timestamp(),

    CONSTRAINT uq_jurisdiction_version UNIQUE (jurisdiction_id, version_no),
    CONSTRAINT ck_jv_change_type CHECK (change_type IN ('ADDED', 'UPDATED', 'HIERARCHY_CHANGED')),
    CONSTRAINT ck_jv_interval CHECK (valid_to IS NULL OR valid_to > valid_from),
    CONSTRAINT ck_jv_not_own_parent CHECK (parent_jurisdiction_id IS DISTINCT FROM jurisdiction_id)
);

-- At most one version is in force indefinitely: a jurisdiction is not two
-- things at once.
CREATE UNIQUE INDEX IF NOT EXISTS uq_jv_one_open
    ON jurisdiction_versions (jurisdiction_id) WHERE valid_to IS NULL;

-- The as-of lookup, and the children-of lookup the hierarchy walk makes.
CREATE INDEX IF NOT EXISTS idx_jv_asof
    ON jurisdiction_versions (jurisdiction_id, valid_from, valid_to);
CREATE INDEX IF NOT EXISTS idx_jv_parent
    ON jurisdiction_versions (parent_jurisdiction_id, valid_from, valid_to)
    WHERE parent_jurisdiction_id IS NOT NULL;

-- History is evidence: a version is closed once (valid_to filled in), never
-- rewritten, never deleted.
CREATE OR REPLACE FUNCTION jurisdiction_versions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'jurisdiction_versions is append-only (% refused)', TG_OP
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.jurisdiction_id IS DISTINCT FROM OLD.jurisdiction_id
       OR NEW.version_no            IS DISTINCT FROM OLD.version_no
       OR NEW.jurisdiction_name     IS DISTINCT FROM OLD.jurisdiction_name
       OR NEW.jurisdiction_type     IS DISTINCT FROM OLD.jurisdiction_type
       OR NEW.parent_jurisdiction_id IS DISTINCT FROM OLD.parent_jurisdiction_id
       OR NEW.authority_type        IS DISTINCT FROM OLD.authority_type
       OR NEW.valid_from            IS DISTINCT FROM OLD.valid_from
       OR NEW.change_type           IS DISTINCT FROM OLD.change_type
       OR NEW.changed_by            IS DISTINCT FROM OLD.changed_by
       OR NEW.recorded_at           IS DISTINCT FROM OLD.recorded_at
       OR (OLD.valid_to IS NOT NULL AND NEW.valid_to IS DISTINCT FROM OLD.valid_to) THEN
        RAISE EXCEPTION 'jurisdiction_versions is append-only: a version can only be closed'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_jurisdiction_versions_guard ON jurisdiction_versions;
CREATE TRIGGER trg_jurisdiction_versions_guard
    BEFORE UPDATE OR DELETE ON jurisdiction_versions
    FOR EACH ROW EXECUTE FUNCTION jurisdiction_versions_guard();

DROP TRIGGER IF EXISTS trg_jurisdiction_versions_no_truncate ON jurisdiction_versions;
CREATE TRIGGER trg_jurisdiction_versions_no_truncate
    BEFORE TRUNCATE ON jurisdiction_versions
    FOR EACH STATEMENT EXECUTE FUNCTION jurisdiction_versions_guard();

-- ── Backfill ────────────────────────────────────────────────────────────────
-- Every existing jurisdiction gets the one version it can be shown to have had:
-- its present attributes over its effective period. Earlier in-place changes
-- were not recorded, so they cannot be reconstructed, and this does not pretend
-- to. Idempotent: a jurisdiction that already has a version is skipped.
INSERT INTO jurisdiction_versions (
    jurisdiction_id, version_no, jurisdiction_name, jurisdiction_type, parent_jurisdiction_id,
    authority_type, valid_from, valid_to, change_type, changed_by, reason)
SELECT j.jurisdiction_id, 1, j.jurisdiction_name, j.jurisdiction_type, j.parent_jurisdiction_id,
       j.authority_type, j.effective_from,
       CASE WHEN j.effective_to IS NULL THEN NULL
            ELSE GREATEST(j.effective_to, j.effective_from + INTERVAL '1 microsecond') END,
       'ADDED', j.created_by_principal_id, 'backfilled by migration 000027'
  FROM jurisdictions j
 WHERE NOT EXISTS (SELECT 1 FROM jurisdiction_versions v WHERE v.jurisdiction_id = j.jurisdiction_id);

-- ── The maintainer ──────────────────────────────────────────────────────────
-- SECURITY DEFINER so the application role needs only SELECT on the table: the
-- history can be written only through this function, as a consequence of a
-- change to jurisdictions.
--
--   INSERT                        -> version 1 (ADDED)
--   name/type/parent/authority    -> close the open version, open the next
--   changes                          (HIERARCHY_CHANGED when the parent moved)
--   effective_to set (retirement) -> close the open version at that instant
--
-- app.valid_from (optional, transaction-local) backdates or postdates the
-- effective instant of an attribute change, for an import that records a change
-- the source says took effect earlier. Without it a change takes effect now.
-- app.reason and app.evidence_ref are recorded on the version when set.
CREATE OR REPLACE FUNCTION jurisdiction_record_version() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_at      TIMESTAMPTZ;
    v_open    RECORD;
    v_next    INTEGER;
    v_change  TEXT;
    v_actor   TEXT;
BEGIN
    v_actor := COALESCE(NULLIF(NEW.updated_by_principal_id, ''), NEW.created_by_principal_id);

    IF TG_OP = 'INSERT' THEN
        INSERT INTO jurisdiction_versions (
            jurisdiction_id, version_no, jurisdiction_name, jurisdiction_type, parent_jurisdiction_id,
            authority_type, valid_from, valid_to, change_type, changed_by, reason, evidence_ref)
        VALUES (NEW.jurisdiction_id, 1, NEW.jurisdiction_name, NEW.jurisdiction_type, NEW.parent_jurisdiction_id,
                NEW.authority_type, NEW.effective_from,
                CASE WHEN NEW.effective_to IS NULL THEN NULL
                     ELSE GREATEST(NEW.effective_to, NEW.effective_from + INTERVAL '1 microsecond') END,
                'ADDED', NEW.created_by_principal_id,
                NULLIF(current_setting('app.reason', true), ''), NULLIF(current_setting('app.evidence_ref', true), ''));
        RETURN NULL;
    END IF;

    SELECT * INTO v_open FROM jurisdiction_versions
     WHERE jurisdiction_id = NEW.jurisdiction_id AND valid_to IS NULL;

    IF NEW.jurisdiction_name      IS DISTINCT FROM OLD.jurisdiction_name
       OR NEW.jurisdiction_type   IS DISTINCT FROM OLD.jurisdiction_type
       OR NEW.parent_jurisdiction_id IS DISTINCT FROM OLD.parent_jurisdiction_id
       OR NEW.authority_type      IS DISTINCT FROM OLD.authority_type THEN

        IF v_open.version_id IS NULL THEN
            RAISE EXCEPTION 'jurisdiction % has no open version to supersede', NEW.jurisdiction_id;
        END IF;
        v_at := COALESCE(NULLIF(current_setting('app.valid_from', true), '')::timestamptz, clock_timestamp());
        v_at := GREATEST(v_at, v_open.valid_from + INTERVAL '1 microsecond');
        v_change := CASE WHEN NEW.parent_jurisdiction_id IS DISTINCT FROM OLD.parent_jurisdiction_id
                         THEN 'HIERARCHY_CHANGED' ELSE 'UPDATED' END;
        SELECT COALESCE(MAX(version_no), 0) + 1 INTO v_next FROM jurisdiction_versions
         WHERE jurisdiction_id = NEW.jurisdiction_id;

        UPDATE jurisdiction_versions SET valid_to = v_at WHERE version_id = v_open.version_id;
        INSERT INTO jurisdiction_versions (
            jurisdiction_id, version_no, jurisdiction_name, jurisdiction_type, parent_jurisdiction_id,
            authority_type, valid_from, valid_to, change_type, changed_by, reason, evidence_ref)
        VALUES (NEW.jurisdiction_id, v_next, NEW.jurisdiction_name, NEW.jurisdiction_type, NEW.parent_jurisdiction_id,
                NEW.authority_type, v_at,
                CASE WHEN NEW.effective_to IS NULL THEN NULL ELSE GREATEST(NEW.effective_to, v_at + INTERVAL '1 microsecond') END,
                v_change, v_actor,
                NULLIF(current_setting('app.reason', true), ''), NULLIF(current_setting('app.evidence_ref', true), ''));
        RETURN NULL;
    END IF;

    IF NEW.effective_to IS DISTINCT FROM OLD.effective_to AND NEW.effective_to IS NOT NULL
       AND v_open.version_id IS NOT NULL THEN
        UPDATE jurisdiction_versions
           SET valid_to = GREATEST(NEW.effective_to, valid_from + INTERVAL '1 microsecond')
         WHERE version_id = v_open.version_id;
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS trg_jurisdiction_record_version ON jurisdictions;
CREATE TRIGGER trg_jurisdiction_record_version
    AFTER INSERT OR UPDATE ON jurisdictions
    FOR EACH ROW EXECUTE FUNCTION jurisdiction_record_version();

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT ON jurisdiction_versions TO app_jurisdiction_rules;
    END IF;
END
$$;
