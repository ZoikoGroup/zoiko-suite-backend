-- 000012_add_com05_billing_invoices.down.sql
DROP TABLE IF EXISTS invoice_lines;
ALTER TABLE IF EXISTS invoice_candidates DROP CONSTRAINT IF EXISTS invoice_candidates_issued_invoice_fk;
DROP TABLE IF EXISTS platform_commercial_invoices;
DROP TABLE IF EXISTS invoice_candidate_lines;
DROP TABLE IF EXISTS invoice_candidates;
DROP FUNCTION IF EXISTS enforce_invoice_candidate_lifecycle();
DROP TABLE IF EXISTS invoice_number_counters;
DROP TABLE IF EXISTS billing_accounts;
