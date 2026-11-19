-- Migration 000023: WFC-04 Cases - DOWN

DROP TRIGGER IF EXISTS cases_updated_at ON cases;
DROP TABLE IF EXISTS cases;