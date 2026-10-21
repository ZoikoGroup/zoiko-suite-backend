-- 000020_row_version_and_rule_immutability.up.sql
--
-- 1. row_version on jurisdictions and jurisdiction_rules, bumped on every
--    UPDATE. It is the aggregate version events carry and the ETag a caller
--    asserts with If-Match (API standard optimistic concurrency).
-- 2. A rule's content is immutable once it leaves DRAFT (ZS-JUR-001 §3:
--    released versions are immutable; a correction is a new version that
--    supersedes it). Status, end date, drift state and audit columns still move.
-- 3. A rule has at most one successor, so the supersedes chain cannot fork.

ALTER TABLE jurisdictions      ADD COLUMN IF NOT EXISTS row_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE jurisdiction_rules ADD COLUMN IF NOT EXISTS row_version BIGINT NOT NULL DEFAULT 1;

CREATE OR REPLACE FUNCTION jur_bump_row_version() RETURNS trigger AS $$
BEGIN
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_jurisdictions_row_version ON jurisdictions;
CREATE TRIGGER trg_jurisdictions_row_version BEFORE UPDATE ON jurisdictions
    FOR EACH ROW EXECUTE FUNCTION jur_bump_row_version();
DROP TRIGGER IF EXISTS trg_jurisdiction_rules_row_version ON jurisdiction_rules;
CREATE TRIGGER trg_jurisdiction_rules_row_version BEFORE UPDATE ON jurisdiction_rules
    FOR EACH ROW EXECUTE FUNCTION jur_bump_row_version();

-- The create path passed rule_version explicitly and nothing ever set it, so
-- API-created rules hold 0. Repaired before the content guard exists.
UPDATE jurisdiction_rules SET rule_version = 1 WHERE rule_version < 1;

CREATE OR REPLACE FUNCTION jur_rule_content_guard() RETURNS trigger AS $$
BEGIN
    IF OLD.rule_status <> 'DRAFT' AND (
           NEW.rule_payload::jsonb IS DISTINCT FROM OLD.rule_payload::jsonb
        OR NEW.rule_name        IS DISTINCT FROM OLD.rule_name
        OR NEW.rule_domain      IS DISTINCT FROM OLD.rule_domain
        OR NEW.rule_code        IS DISTINCT FROM OLD.rule_code
        OR NEW.jurisdiction_id  IS DISTINCT FROM OLD.jurisdiction_id
        OR NEW.effective_from   IS DISTINCT FROM OLD.effective_from
        OR NEW.precedence_level IS DISTINCT FROM OLD.precedence_level
        OR NEW.rule_version     IS DISTINCT FROM OLD.rule_version
        OR NEW.supersedes_rule_id IS DISTINCT FROM OLD.supersedes_rule_id) THEN
        RAISE EXCEPTION 'rule % is past DRAFT; its content is immutable (create a new version that supersedes it)',
            OLD.jurisdiction_rule_id USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_jurisdiction_rules_content_guard ON jurisdiction_rules;
CREATE TRIGGER trg_jurisdiction_rules_content_guard BEFORE UPDATE ON jurisdiction_rules
    FOR EACH ROW EXECUTE FUNCTION jur_rule_content_guard();

CREATE UNIQUE INDEX IF NOT EXISTS uq_jurisdiction_rules_single_successor
    ON jurisdiction_rules (supersedes_rule_id) WHERE supersedes_rule_id IS NOT NULL;
