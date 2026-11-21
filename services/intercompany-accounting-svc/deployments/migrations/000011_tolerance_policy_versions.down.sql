-- Migration: 000011_tolerance_policy_versions.down.sql
--
-- Manual rollback only, and refused while any tolerance policy version is
-- recorded — same posture as 000009's command_idempotency down: dropping the
-- table or the column would forget which tolerance a past attempt was
-- actually evaluated under, which is exactly the evidence this migration
-- exists to keep.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM intercompany_tolerance_policy_versions) THEN
        RAISE EXCEPTION 'cannot roll back 000011: tolerance policy versions exist';
    END IF;
    IF EXISTS (SELECT 1 FROM intercompany_match_attempts WHERE tolerance_version <> 0) THEN
        RAISE EXCEPTION 'cannot roll back 000011: a match attempt recorded a non-default tolerance_version';
    END IF;
END $$;

ALTER TABLE intercompany_match_attempts DROP COLUMN IF EXISTS tolerance_version;
DROP TABLE IF EXISTS intercompany_tolerance_policy_versions;
