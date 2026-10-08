-- 000026_notice_event_sequence.up.sql
-- A notice's transitions are written in one transaction, so they share a timestamp (now() is the
-- transaction start) and ordering by time alone is ambiguous. seq gives the history its true order.
ALTER TABLE regulated_notice_events ADD COLUMN IF NOT EXISTS seq BIGINT GENERATED ALWAYS AS IDENTITY;
CREATE INDEX IF NOT EXISTS idx_notice_events_seq ON regulated_notice_events (tenant_id, notice_id, seq);
