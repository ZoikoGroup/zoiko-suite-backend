-- 000034 down: drop the row versions and their trigger. A version is only a
-- concurrency token; no history is lost with it.
DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'regulatory_sources', 'interpretation_records', 'jurisdiction_pack_versions', 'pack_signing_keys',
        'regulatory_calendar_versions', 'obligation_rules', 'payment_term_versions', 'jurisdiction_rollouts',
        'remediation_cases', 'source_change_notices', 'jurisdiction_import_batches', 'jurisdiction_aliases',
        'regulatory_submissions', 'pack_hotfixes']
    LOOP
        IF to_regclass(t) IS NOT NULL THEN
            EXECUTE format('DROP TRIGGER IF EXISTS zzz_row_version ON %I', t);
            EXECUTE format('ALTER TABLE %I DROP COLUMN IF EXISTS row_version', t);
        END IF;
    END LOOP;
END
$$;
DROP FUNCTION IF EXISTS jur_bump_aggregate_row_version();
