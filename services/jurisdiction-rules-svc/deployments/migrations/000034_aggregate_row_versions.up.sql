-- 000034_aggregate_row_versions.up.sql
--
-- API-3 / R6 (and the REF-01, REF-10 contract: "all protected changes require
-- authoritative current version"): every aggregate a command changes carries a
-- row_version, the ETag its reads return and the If-Match its commands check.
-- jurisdictions and jurisdiction_rules have had one since 000020; these are the
-- other fourteen aggregates with commands.
--
-- The version moves on every real change and only then: the trigger compares the
-- whole row, so a no-op UPDATE (an idempotent replay) leaves it alone. It is named
-- to fire last among the BEFORE UPDATE triggers (they fire in name order), so the
-- immutability guards that compare OLD and NEW never see a version change.
--
-- Registry events carry the version as aggregate_version (Governance audit J-12).

CREATE OR REPLACE FUNCTION jur_bump_aggregate_row_version() RETURNS trigger AS $$
BEGIN
    IF ROW(NEW.*) IS DISTINCT FROM ROW(OLD.*) THEN
        NEW.row_version := OLD.row_version + 1;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

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
        EXECUTE format('ALTER TABLE %I ADD COLUMN IF NOT EXISTS row_version BIGINT NOT NULL DEFAULT 1', t);
        EXECUTE format('DROP TRIGGER IF EXISTS zzz_row_version ON %I', t);
        EXECUTE format('CREATE TRIGGER zzz_row_version BEFORE UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION jur_bump_aggregate_row_version()', t);
    END LOOP;
END
$$;
