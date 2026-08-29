DROP TRIGGER IF EXISTS trg_reject_receipt_accounting_events_mutation ON receipt_accounting_events;
DROP TRIGGER IF EXISTS trg_reject_receipt_reversals_mutation ON receipt_reversals;
DROP TRIGGER IF EXISTS trg_reject_receipt_evidence_mutation ON receipt_evidence;
DROP TRIGGER IF EXISTS trg_reject_receipt_mutation ON goods_service_receipts;
DROP FUNCTION IF EXISTS reject_evidence_mutation();
DROP FUNCTION IF EXISTS reject_receipt_mutation();
