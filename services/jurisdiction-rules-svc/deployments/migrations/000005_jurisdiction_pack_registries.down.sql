-- 000005_jurisdiction_pack_registries.down.sql
-- Reverses ZS-JUR-001 Wave 0. Drops registry data: only safe before any pack
-- or source has been recorded in an environment that matters.

DROP TABLE IF EXISTS rule_sources;
DROP TABLE IF EXISTS pack_dependencies;
DROP TABLE IF EXISTS pack_version_regimes;
DROP TABLE IF EXISTS pack_version_jurisdictions;
DROP TABLE IF EXISTS jurisdiction_pack_versions;
DROP TABLE IF EXISTS jurisdiction_packs;
DROP TABLE IF EXISTS interpretation_sources;

DROP TRIGGER IF EXISTS trg_jurisdiction_rules_provenance_guard ON jurisdiction_rules;
ALTER TABLE jurisdiction_rules
    DROP COLUMN IF EXISTS published_on,
    DROP COLUMN IF EXISTS precedence,
    DROP COLUMN IF EXISTS supersedes_rule_id,
    DROP COLUMN IF EXISTS interpretation_id,
    DROP COLUMN IF EXISTS regime_id;

DROP TABLE IF EXISTS interpretation_records;
DROP TABLE IF EXISTS regulatory_sources;
DROP TABLE IF EXISTS regulatory_regimes;

DROP FUNCTION IF EXISTS jur_rule_sources_delete_guard();
DROP FUNCTION IF EXISTS jur_rule_sources_guard();
DROP FUNCTION IF EXISTS jur_rule_provenance_guard();
DROP FUNCTION IF EXISTS jur_pack_children_guard();
DROP FUNCTION IF EXISTS jur_pack_version_guard();
DROP FUNCTION IF EXISTS jur_interpretation_sources_guard();
DROP FUNCTION IF EXISTS jur_interpretation_guard();
DROP FUNCTION IF EXISTS jur_source_guard();
DROP FUNCTION IF EXISTS jur_forbid_delete();
