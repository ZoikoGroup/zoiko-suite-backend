-- Migration: 000006_pair_closed_state_transitions.down.sql
--
-- Manual rollback only, and refused while any pair is CLOSED: 000005's
-- state set has no CLOSED, and silently reopening a closed pair would
-- un-close it. Loses the transition history and every resolution outcome.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM intercompany_entries WHERE match_status = 'CLOSED') THEN
        RAISE EXCEPTION 'cannot roll back 000006: CLOSED pairs exist';
    END IF;
END $$;

DROP TABLE IF EXISTS intercompany_pair_transitions;
DROP FUNCTION IF EXISTS reject_immutable_row();

ALTER TABLE intercompany_entries
    DROP CONSTRAINT IF EXISTS chk_resolved_has_outcome,
    DROP CONSTRAINT IF EXISTS chk_closed_is_attributed,
    DROP CONSTRAINT IF EXISTS chk_match_status;
ALTER TABLE intercompany_entries
    ADD CONSTRAINT chk_match_status CHECK (match_status IN (
        'UNMATCHED', 'AWAITING_COUNTERPARTY', 'MATCHED', 'MISMATCH', 'DISPUTED', 'RESOLVED'
    ));
ALTER TABLE intercompany_entries
    DROP COLUMN IF EXISTS close_reason,
    DROP COLUMN IF EXISTS closed_by_principal_id,
    DROP COLUMN IF EXISTS closed_at,
    DROP COLUMN IF EXISTS resolution_outcome;
