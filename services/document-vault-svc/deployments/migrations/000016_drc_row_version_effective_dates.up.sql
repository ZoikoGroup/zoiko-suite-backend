-- ZS-SVC-S-001 §3.2 row_version: "Optimistic-concurrency token; increments on
-- mutable metadata changes." A persistent integer, set by the database on every
-- UPDATE so no client or code path can set or skip it.
ALTER TABLE documents ADD COLUMN IF NOT EXISTS row_version BIGINT NOT NULL DEFAULT 1;

CREATE OR REPLACE FUNCTION bump_document_row_version() RETURNS TRIGGER AS $$
BEGIN
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_bump_document_row_version ON documents;
CREATE TRIGGER trg_bump_document_row_version
    BEFORE UPDATE ON documents
    FOR EACH ROW EXECUTE FUNCTION bump_document_row_version();

-- ZS-SVC-S-001 §5.2 effective_from / effective_to: "Version applicability
-- dates." Optional; a rule with neither applies at every instant, as before.
ALTER TABLE retention_rule_versions ADD COLUMN IF NOT EXISTS effective_from TIMESTAMPTZ;
ALTER TABLE retention_rule_versions ADD COLUMN IF NOT EXISTS effective_to TIMESTAMPTZ;
ALTER TABLE retention_rule_versions DROP CONSTRAINT IF EXISTS retention_rule_effective_window;
ALTER TABLE retention_rule_versions ADD CONSTRAINT retention_rule_effective_window
    CHECK (effective_from IS NULL OR effective_to IS NULL OR effective_to > effective_from);

-- Like every other term of an approved rule, the applicability window is
-- immutable once the rule leaves DRAFT.
CREATE OR REPLACE FUNCTION reject_retention_rule_window_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status <> 'DRAFT' AND (
        NEW.effective_from IS DISTINCT FROM OLD.effective_from
        OR NEW.effective_to IS DISTINCT FROM OLD.effective_to
    ) THEN
        RAISE EXCEPTION 'retention_rule_version % is no longer DRAFT; its terms are immutable', OLD.retention_rule_version_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_reject_retention_rule_window_mutation ON retention_rule_versions;
CREATE TRIGGER trg_reject_retention_rule_window_mutation
    BEFORE UPDATE ON retention_rule_versions
    FOR EACH ROW EXECUTE FUNCTION reject_retention_rule_window_mutation();
