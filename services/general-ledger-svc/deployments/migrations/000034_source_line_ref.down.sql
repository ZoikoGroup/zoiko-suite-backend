DROP INDEX IF EXISTS idx_journal_lines_source_line;
ALTER TABLE journal_lines DROP COLUMN IF EXISTS source_line_ref;
