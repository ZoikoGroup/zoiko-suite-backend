-- Migration: 000008_ap05_state_dimensions.down.sql
BEGIN;
DROP TABLE IF EXISTS payable_creation_requests;
DROP TABLE IF EXISTS ap_command_idempotency;
DROP TABLE IF EXISTS invoice_correction_links;
DROP TABLE IF EXISTS invoice_history;
DROP TABLE IF EXISTS invoice_duplicate_assessments;
DROP TRIGGER IF EXISTS trg_vendor_invoice_lines_guard ON vendor_invoice_lines;
DROP TRIGGER IF EXISTS trg_vendor_invoices_guard ON vendor_invoices;
DROP FUNCTION IF EXISTS vendor_invoice_lines_guard();
DROP FUNCTION IF EXISTS vendor_invoices_guard();
DROP FUNCTION IF EXISTS ap_append_only_guard();
ALTER TABLE vendor_invoices
    DROP CONSTRAINT IF EXISTS chk_vi_intake, DROP CONSTRAINT IF EXISTS chk_vi_match,
    DROP CONSTRAINT IF EXISTS chk_vi_approval, DROP CONSTRAINT IF EXISTS chk_vi_accounting,
    DROP CONSTRAINT IF EXISTS chk_vi_settlement, DROP CONSTRAINT IF EXISTS chk_vi_hold,
    DROP CONSTRAINT IF EXISTS chk_vi_doctype, DROP CONSTRAINT IF EXISTS chk_vi_dup,
    DROP CONSTRAINT IF EXISTS chk_vi_tax, DROP CONSTRAINT IF EXISTS chk_vi_payee;
DROP INDEX IF EXISTS idx_vendor_invoices_near_dup;
DROP INDEX IF EXISTS idx_vendor_invoices_amount_dup;
ALTER TABLE vendor_invoices
    DROP COLUMN IF EXISTS version, DROP COLUMN IF EXISTS document_type,
    DROP COLUMN IF EXISTS intake_state, DROP COLUMN IF EXISTS match_state,
    DROP COLUMN IF EXISTS approval_state, DROP COLUMN IF EXISTS accounting_state,
    DROP COLUMN IF EXISTS settlement_state, DROP COLUMN IF EXISTS hold_state,
    DROP COLUMN IF EXISTS hold_reason, DROP COLUMN IF EXISTS source_hash,
    DROP COLUMN IF EXISTS source_payload, DROP COLUMN IF EXISTS attachment_hash,
    DROP COLUMN IF EXISTS source_channel, DROP COLUMN IF EXISTS source_accepted_at,
    DROP COLUMN IF EXISTS invoice_number_normalized, DROP COLUMN IF EXISTS duplicate_state,
    DROP COLUMN IF EXISTS quarantine_reason, DROP COLUMN IF EXISTS submitted_by_principal_id,
    DROP COLUMN IF EXISTS submitted_at, DROP COLUMN IF EXISTS rejected_by_principal_id,
    DROP COLUMN IF EXISTS rejected_at, DROP COLUMN IF EXISTS reject_reason,
    DROP COLUMN IF EXISTS tax_state, DROP COLUMN IF EXISTS tax_provenance,
    DROP COLUMN IF EXISTS tax_result_hash, DROP COLUMN IF EXISTS tax_verified_at,
    DROP COLUMN IF EXISTS withholding_ref, DROP COLUMN IF EXISTS withholding_determination_id,
    DROP COLUMN IF EXISTS withholding_provenance, DROP COLUMN IF EXISTS extracted_bank_details,
    DROP COLUMN IF EXISTS payee_state, DROP COLUMN IF EXISTS payee_check,
    DROP COLUMN IF EXISTS supplier_profile_id, DROP COLUMN IF EXISTS supplier_profile_version,
    DROP COLUMN IF EXISTS po_revision, DROP COLUMN IF EXISTS match_required,
    DROP COLUMN IF EXISTS match_cleared, DROP COLUMN IF EXISTS match_run_id,
    DROP COLUMN IF EXISTS payable_id, DROP COLUMN IF EXISTS accounting_event_id;
COMMIT;
