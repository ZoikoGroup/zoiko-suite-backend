-- Migration: 000400_ar04_customer_invoices.down.sql
DROP TABLE IF EXISTS ar04_invoice_history;
DROP TABLE IF EXISTS ar04_correction_links;
DROP TABLE IF EXISTS ar04_cancellation_requests;
DROP TABLE IF EXISTS ar04_invoice_approvals;
DROP TABLE IF EXISTS ar04_invoice_tax_determinations;
DROP TABLE IF EXISTS ar04_invoice_lines;
DROP TABLE IF EXISTS ar04_invoices;
DROP TABLE IF EXISTS ar04_number_counters;
DROP TABLE IF EXISTS ar04_numbering_profiles;
DROP FUNCTION IF EXISTS ar04_guard_cancellations();
DROP FUNCTION IF EXISTS ar04_guard_children();
DROP FUNCTION IF EXISTS ar04_guard_invoices();
DROP FUNCTION IF EXISTS ar04_guard_counters();
