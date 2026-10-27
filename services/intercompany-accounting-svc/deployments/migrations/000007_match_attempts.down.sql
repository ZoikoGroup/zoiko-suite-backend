-- Migration: 000007_match_attempts.down.sql
--
-- Manual rollback only. Before dropping the attempts, writes each pair's
-- latest attempt message back to mismatch_reason (the column 000007 stopped
-- writing), so the pre-000007 binary reads the same reason this one did.
-- Every attempt's structured detail is lost.

-- The write-back reads every tenant's attempts with no tenant context.
ALTER TABLE intercompany_match_attempts NO FORCE ROW LEVEL SECURITY;

UPDATE intercompany_entries e
SET mismatch_reason = (
    SELECT a.message FROM intercompany_match_attempts a
    WHERE a.tenant_id = e.tenant_id AND a.intercompany_entry_id = e.intercompany_entry_id
    ORDER BY a.attempt_id DESC LIMIT 1)
WHERE EXISTS (
    SELECT 1 FROM intercompany_match_attempts a
    WHERE a.tenant_id = e.tenant_id AND a.intercompany_entry_id = e.intercompany_entry_id);

DROP TABLE IF EXISTS intercompany_match_attempts;
