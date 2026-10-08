-- Migration: 000015_add_soft_close_override_reason.down.sql

ALTER TABLE journal_headers
    DROP COLUMN soft_close_override_reason;