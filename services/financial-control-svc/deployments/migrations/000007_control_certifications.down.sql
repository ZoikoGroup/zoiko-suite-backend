-- Migration: 000007_control_certifications.down.sql
--
-- Manual rollback only, and refused while any certification is recorded:
-- dropping the table would lose the authority snapshot, accepted exceptions
-- and assessed exposure Invariant 9 requires retained, with no way to
-- reconstruct them (the authorization decision that granted the certifier
-- is not stored anywhere else). The runs' states and transition history are
-- untouched either way.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM control_certifications) THEN
        RAISE EXCEPTION 'cannot roll back 000007: recorded certifications exist';
    END IF;
END $$;

DROP TABLE IF EXISTS control_certifications;
