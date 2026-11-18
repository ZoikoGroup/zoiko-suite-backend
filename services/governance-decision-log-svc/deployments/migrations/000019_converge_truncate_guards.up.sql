-- Migration: 000019_converge_truncate_guards.up.sql
--
-- Two lineages of 000011 exist. Databases migrated before 12 Oct 2026 ran its
-- first version: function block_truncate() and *_block_truncate triggers on
-- governance_decisions, replay_manifests, idempotency_keys and outbox. The file
-- was then rewritten in place (reject_truncate(), *_no_truncate on the two
-- evidence tables only), so a fresh database and an old one disagree: the old
-- one also refuses TRUNCATE on idempotency_keys and outbox, and 000011's down
-- file cannot remove its triggers. migrate.py reports the file as "edited after
-- applying". This brings an old-lineage database to exactly what the current
-- 000011 creates, and is a no-op on a fresh one.

BEGIN;

DROP TRIGGER IF EXISTS governance_decisions_block_truncate ON governance_decisions;
DROP TRIGGER IF EXISTS replay_manifests_block_truncate ON replay_manifests;
DROP TRIGGER IF EXISTS idempotency_keys_block_truncate ON idempotency_keys;
DROP TRIGGER IF EXISTS outbox_block_truncate ON outbox;
DROP FUNCTION IF EXISTS block_truncate();

CREATE OR REPLACE FUNCTION reject_truncate() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: TRUNCATE is not permitted on this table',
        TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS governance_decisions_no_truncate ON governance_decisions;
CREATE TRIGGER governance_decisions_no_truncate
    BEFORE TRUNCATE ON governance_decisions
    FOR EACH STATEMENT EXECUTE FUNCTION reject_truncate();

DROP TRIGGER IF EXISTS replay_manifests_no_truncate ON replay_manifests;
CREATE TRIGGER replay_manifests_no_truncate
    BEFORE TRUNCATE ON replay_manifests
    FOR EACH STATEMENT EXECUTE FUNCTION reject_truncate();

COMMIT;
