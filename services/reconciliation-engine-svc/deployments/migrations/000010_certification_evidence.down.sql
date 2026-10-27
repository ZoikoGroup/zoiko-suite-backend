-- Migration: 000010_certification_evidence.down.sql
--
-- Reverses 000010. A certification's authority, the populations it was sealed
-- over and its manual matches, and a manual match's evidence, are evidence
-- with no 000009 representation; the manifest hash of a certification sealed
-- under 000010 covers them, so dropping them would leave a hash nothing can be
-- checked against. The down refuses while any exists (same posture as 000007
-- and 000009's downs).

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM certifications WHERE authority_snapshot IS NOT NULL)
        OR EXISTS (SELECT 1 FROM match_results WHERE evidence IS NOT NULL)
    THEN
        RAISE EXCEPTION 'cannot roll back 000010: a certification or manual match records evidence that has no earlier representation';
    END IF;
END $$;

ALTER TABLE match_results DROP CONSTRAINT IF EXISTS match_results_manual_evidence_check;
ALTER TABLE match_results DROP COLUMN IF EXISTS evidence;

ALTER TABLE certifications DROP CONSTRAINT IF EXISTS certifications_evidence_check;
ALTER TABLE certifications
    DROP COLUMN IF EXISTS manual_matches,
    DROP COLUMN IF EXISTS populations,
    DROP COLUMN IF EXISTS authority_snapshot;
