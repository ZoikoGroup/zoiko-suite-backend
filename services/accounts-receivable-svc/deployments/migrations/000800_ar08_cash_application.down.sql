-- Migration: 000800_ar08_cash_application.down.sql
DROP TABLE IF EXISTS ar08_reconciliations;
DROP TABLE IF EXISTS ar08_application_lines;
DROP TABLE IF EXISTS ar08_applications;
DROP TABLE IF EXISTS ar08_manual_matches;
DROP TABLE IF EXISTS ar08_candidates;
DROP TABLE IF EXISTS ar08_batch_receipts;
DROP TABLE IF EXISTS ar08_batches;
DROP TABLE IF EXISTS ar08_receipts;
DROP FUNCTION IF EXISTS ar08_guard_receipt();
DROP FUNCTION IF EXISTS ar08_guard_application();
DROP FUNCTION IF EXISTS ar08_guard_match();
