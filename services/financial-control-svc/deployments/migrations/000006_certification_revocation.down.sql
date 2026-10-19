-- Migration: 000006_certification_revocation.down.sql
--
-- Manual rollback only, and refused while any run's certification is
-- REVOKED: 000001's rule (lifecycle CERTIFIED implies certification
-- CERTIFIED) cannot hold for it, and silently re-certifying a revoked run
-- would undo the revocation.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM control_runs WHERE certification_state = 'REVOKED') THEN
        RAISE EXCEPTION 'cannot roll back 000006: revoked certifications exist';
    END IF;
END $$;

DROP TABLE IF EXISTS control_certification_revocations;

ALTER TABLE control_runs
    DROP CONSTRAINT IF EXISTS chk_revoked_follows_certification,
    DROP CONSTRAINT IF EXISTS chk_certified_lifecycle_certification;
ALTER TABLE control_runs
    ADD CHECK (lifecycle_state <> 'CERTIFIED' OR certification_state = 'CERTIFIED');
