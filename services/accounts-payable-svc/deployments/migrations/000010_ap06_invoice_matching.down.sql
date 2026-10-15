-- Migration: 000010_ap06_invoice_matching.down.sql
DROP TRIGGER IF EXISTS trg_vendor_invoices_match_gate ON vendor_invoices;
DROP FUNCTION IF EXISTS vendor_invoices_match_gate();
ALTER TABLE vendor_invoices DROP CONSTRAINT IF EXISTS chk_vi_match_cleared;

DROP TABLE IF EXISTS invoice_match_exceptions;
DROP TABLE IF EXISTS invoice_match_lines;
DROP TABLE IF EXISTS invoice_match_runs;
DROP TABLE IF EXISTS match_policy_versions;
DROP FUNCTION IF EXISTS invoice_match_exceptions_guard();
DROP FUNCTION IF EXISTS invoice_match_runs_guard();
