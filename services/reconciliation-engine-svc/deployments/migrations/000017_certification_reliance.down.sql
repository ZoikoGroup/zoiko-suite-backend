-- Refused while any reliance is recorded: dropping them would erase the record of what rested on a certification,
-- which is exactly what a later revocation needs to name.
DO $$
BEGIN
    IF to_regclass('reconciliation_certification_reliances') IS NOT NULL
       AND EXISTS (SELECT 1 FROM reconciliation_certification_reliances) THEN
        RAISE EXCEPTION '000017 down refused: certification reliances have been recorded';
    END IF;
END
$$;

DROP TABLE IF EXISTS reconciliation_certification_reliances;
DROP FUNCTION IF EXISTS enforce_recon_reliance_rules();
