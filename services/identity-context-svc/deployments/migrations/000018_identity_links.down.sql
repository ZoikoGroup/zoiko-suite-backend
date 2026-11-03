-- Migration: 000018_identity_links.down.sql
--
-- Revert the IC-3 identity links and merges. Refused while a role references
-- either table's grants or a merge has been recorded (defensive).

DO $$
DECLARE
    v_merge_count INT;
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'identity_merges') THEN
        SELECT count(*) INTO v_merge_count FROM identity_merges;
        IF v_merge_count > 0 THEN
            RAISE EXCEPTION '000018 down refused: % identity merge(s) recorded', v_merge_count;
        END IF;
    END IF;
END
$$;

ALTER TABLE identity_links DROP POLICY IF EXISTS tenant_isolation_policy;
ALTER TABLE identity_merges DROP POLICY IF EXISTS tenant_isolation_policy;

DROP TABLE IF EXISTS identity_merges;
DROP TABLE IF EXISTS identity_links;
