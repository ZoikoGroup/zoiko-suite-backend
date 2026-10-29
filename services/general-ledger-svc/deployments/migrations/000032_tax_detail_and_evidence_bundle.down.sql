DROP INDEX IF EXISTS idx_journal_lines_tax_detail;
DROP INDEX IF EXISTS idx_journal_headers_evidence_bundle;
DROP TRIGGER IF EXISTS journal_headers_evidence_bundle ON journal_headers;
DROP FUNCTION IF EXISTS journal_header_evidence_bundle();
DROP FUNCTION IF EXISTS evidence_bundle_digest(TEXT[]);
ALTER TABLE journal_headers DROP COLUMN IF EXISTS evidence_bundle_id;
ALTER TABLE journal_lines DROP COLUMN IF EXISTS tax_detail_ref;
