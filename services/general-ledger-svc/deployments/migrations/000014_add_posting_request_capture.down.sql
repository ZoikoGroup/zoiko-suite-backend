-- Migration: 000014_add_posting_request_capture.down.sql
-- Drops the captured requests: executions that failed before a journal was
-- written become unreplayable again.

ALTER TABLE posting_executions DROP COLUMN IF EXISTS reprocess_claimed_at;
ALTER TABLE posting_executions DROP COLUMN IF EXISTS request_payload;
