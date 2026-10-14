-- Migration: 000600_ar06_corrections.down.sql
DROP TABLE IF EXISTS ar06_correction_history;
DROP TABLE IF EXISTS ar06_replacement_links;
DROP TABLE IF EXISTS ar06_correction_tax_determinations;
DROP TABLE IF EXISTS ar06_correction_lines;
DROP TABLE IF EXISTS ar06_corrections;
DROP TABLE IF EXISTS ar06_source_locks;
DROP TABLE IF EXISTS ar06_number_counters;
DROP TABLE IF EXISTS ar06_numbering_profiles;
DROP FUNCTION IF EXISTS ar06_guard_children();
DROP FUNCTION IF EXISTS ar06_guard_corrections();
DROP FUNCTION IF EXISTS ar06_guard_counters();
