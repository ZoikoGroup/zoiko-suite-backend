-- Migration: 000007_add_consolidation_eliminations.down.sql
--
-- Manual rollback only, and refused while any elimination is recorded: each
-- row is the only record of what a past run subtracted from group balances
-- and why, and nothing else holds it. Same posture as 000006's down.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM consolidation_eliminations) THEN
        RAISE EXCEPTION 'cannot roll back 000007: consolidation eliminations exist';
    END IF;
END $$;

DROP TABLE IF EXISTS consolidation_eliminations;
DROP FUNCTION IF EXISTS reject_consolidation_elimination_mutation();
