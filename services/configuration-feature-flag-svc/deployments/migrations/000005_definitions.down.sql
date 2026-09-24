-- Migration: 000005_definitions.down.sql
--
-- Drops the key registry — the immutable version table first, then the
-- working declaration table — and its RLS policies. The keys config_entries /
-- feature_flags hold are untouched; they simply become free-form again until
-- this migration is re-applied.

DROP POLICY IF EXISTS definition_versions_read_all_write_admin ON config_definition_versions;
DROP POLICY IF EXISTS definitions_read_all_write_admin ON config_definitions;

DROP TABLE IF EXISTS config_definition_versions;
DROP TABLE IF EXISTS config_definitions;