-- Global Accounting Kernel (standard §8.2): a journal line links back to the invoice, order, payroll, asset or other source line it
-- was made from, so the lineage runs down to the line of the source document and not only to the document.
ALTER TABLE journal_lines
    ADD COLUMN source_line_ref VARCHAR(200);
CREATE INDEX idx_journal_lines_source_line ON journal_lines (tenant_id, source_line_ref) WHERE source_line_ref IS NOT NULL;
