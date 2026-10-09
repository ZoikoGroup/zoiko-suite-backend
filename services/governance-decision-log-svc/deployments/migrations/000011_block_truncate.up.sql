-- Migration: 000011_block_truncate.up.sql
-- Block TRUNCATE on append-only evidence tables.
-- TRUNCATE bypasses UPDATE/DELETE triggers, so a separate trigger is needed.

CREATE OR REPLACE FUNCTION reject_truncate() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: TRUNCATE is not permitted on this table',
        TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER governance_decisions_no_truncate
    BEFORE TRUNCATE ON governance_decisions
    FOR EACH STATEMENT EXECUTE FUNCTION reject_truncate();

CREATE TRIGGER replay_manifests_no_truncate
    BEFORE TRUNCATE ON replay_manifests
    FOR EACH STATEMENT EXECUTE FUNCTION reject_truncate();