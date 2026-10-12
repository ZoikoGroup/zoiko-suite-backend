DROP TABLE IF EXISTS po_progress_pushes;
DROP TRIGGER IF EXISTS trg_guard_posting_request ON accounting_posting_requests;
DROP TABLE IF EXISTS accounting_posting_requests;
DROP FUNCTION IF EXISTS guard_posting_request();
