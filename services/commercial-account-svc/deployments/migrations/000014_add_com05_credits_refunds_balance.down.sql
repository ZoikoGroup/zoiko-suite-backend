-- 000014_add_com05_credits_refunds_balance.down.sql
DROP TABLE IF EXISTS refund_requests;
DROP FUNCTION IF EXISTS enforce_refund_request_lifecycle();
DROP TABLE IF EXISTS write_offs;
DROP TABLE IF EXISTS credit_notes;
