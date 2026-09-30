-- Migration: 000011_block_truncate.up.sql
--
-- Blocks TRUNCATE on governance_decisions and replay_manifests tables
-- per GOV-07 "no general-purpose admin rewrite" and ZS-DATA-GOV-001.
-- The existing BEFORE UPDATE OR DELETE triggers don't block TRUNCATE.

CREATE OR REPLACE FUNCTION block_truncate() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: TRUNCATE is not permitted on this table',
        TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

-- Create TRUNCATE trigger on governance_decisions
DROP TRIGGER IF EXISTS governance_decisions_block_truncate ON governance_decisions;
CREATE TRIGGER governance_decisions_block_truncate
    BEFORE TRUNCATE ON governance_decisions
    FOR EACH STATEMENT EXECUTE FUNCTION block_truncate();

-- Create TRUNCATE trigger on replay_manifests
DROP TRIGGER IF EXISTS replay_manifests_block_truncate ON replay_manifests;
CREATE TRIGGER replay_manifests_block_truncate
    BEFORE TRUNCATE ON replay_manifests
    FOR EACH STATEMENT EXECUTE FUNCTION block_truncate();

-- Create TRUNCATE trigger on idempotency_keys
DROP TRIGGER IF EXISTS idempotency_keys_block_truncate ON idempotency_keys;
CREATE TRIGGER idempotency_keys_block_truncate
    BEFORE TRUNCATE ON idempotency_keys
    FOR EACH STATEMENT EXECUTE FUNCTION block_truncate();

-- Create TRUNCATE trigger on outbox
DROP TRIGGER IF EXISTS outbox_block_truncate ON outbox;
CREATE TRIGGER outbox_block_truncate
    BEFORE TRUNCATE ON outbox
    FOR EACH STATEMENT EXECUTE FUNCTION block_truncate();