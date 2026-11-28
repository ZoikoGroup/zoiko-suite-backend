-- Migration 000014: WF-3 Deadline Clocks (WFC-05) - DOWN
-- Drops the obligation_clocks table

DROP TRIGGER IF EXISTS obligation_clocks_updated_at ON obligation_clocks;
DROP TABLE IF EXISTS obligation_clocks;