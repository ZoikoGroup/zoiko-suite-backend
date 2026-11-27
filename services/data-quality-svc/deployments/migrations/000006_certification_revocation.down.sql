-- Refused while any certification has been revoked: dropping the table would silently un-revoke it.
DO $$
BEGIN
    IF to_regclass('dq_certification_revocations') IS NOT NULL
       AND EXISTS (SELECT 1 FROM dq_certification_revocations) THEN
        RAISE EXCEPTION '000006 down refused: certifications have been revoked';
    END IF;
END
$$;
DROP TABLE IF EXISTS dq_certification_revocations;
DROP FUNCTION IF EXISTS enforce_dq_revocation_immutability();
DROP FUNCTION IF EXISTS dq_refs_nonblank(TEXT[]);
DROP FUNCTION IF EXISTS dq_refs_nonblank(TEXT[]);
