-- Migration: 000200_ar02_quotes.down.sql
DROP TABLE IF EXISTS ar02_quote_history;
DROP TABLE IF EXISTS ar02_quote_approvals;
DROP TABLE IF EXISTS ar02_quote_tax_estimates;
DROP TABLE IF EXISTS ar02_quote_lines;
DROP TABLE IF EXISTS ar02_quote_versions;
DROP TABLE IF EXISTS ar02_quotes;
DROP TABLE IF EXISTS ar02_quote_counters;
DROP FUNCTION IF EXISTS ar02_check_version_seq();
