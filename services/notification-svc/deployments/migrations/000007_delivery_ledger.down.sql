-- 000007_delivery_ledger.down.sql
-- Revert Canonical Delivery Ledger tables

DROP TABLE IF EXISTS delivery_events CASCADE;
DROP TABLE IF EXISTS delivery_attempts CASCADE;
DROP TABLE IF EXISTS message_renders CASCADE;
DROP TABLE IF EXISTS message_intents CASCADE;
