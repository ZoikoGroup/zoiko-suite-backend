ALTER TABLE accounting_events DROP COLUMN IF EXISTS supersedes_event_id;
DROP TRIGGER IF EXISTS journal_lines_posted_guard ON journal_lines;
DROP TRIGGER IF EXISTS journal_headers_posted_guard ON journal_headers;
DROP FUNCTION IF EXISTS journal_line_posted_guard();
DROP FUNCTION IF EXISTS journal_header_posted_guard();
DROP TABLE IF EXISTS recurring_journal_templates;
ALTER TABLE chart_of_accounts DROP COLUMN IF EXISTS applicable_book_ids, DROP COLUMN IF EXISTS currency_restriction;
DROP TABLE IF EXISTS event_posting_links;
ALTER TABLE posting_executions
    DROP COLUMN IF EXISTS accounting_event_id, DROP COLUMN IF EXISTS accounting_book_id, DROP COLUMN IF EXISTS posting_basis,
    DROP COLUMN IF EXISTS posting_rule_set_id, DROP COLUMN IF EXISTS rule_version, DROP COLUMN IF EXISTS proposal_hash,
    DROP COLUMN IF EXISTS ledger_watermark, DROP COLUMN IF EXISTS journal_event_id;
DROP TABLE IF EXISTS journal_number_counters;
ALTER TABLE journal_lines
    DROP COLUMN IF EXISTS transaction_amount, DROP COLUMN IF EXISTS transaction_currency, DROP COLUMN IF EXISTS exchange_rate,
    DROP COLUMN IF EXISTS rate_ref, DROP COLUMN IF EXISTS posting_rule_line_ref;
ALTER TABLE journal_headers
    DROP CONSTRAINT IF EXISTS journal_headers_correction_kind, DROP CONSTRAINT IF EXISTS journal_headers_purpose;
DROP INDEX IF EXISTS idx_journal_headers_source_event;
DROP INDEX IF EXISTS uq_journal_headers_number;
ALTER TABLE journal_headers
    DROP COLUMN IF EXISTS journal_number, DROP COLUMN IF EXISTS accounting_book_id, DROP COLUMN IF EXISTS source_accounting_event_id,
    DROP COLUMN IF EXISTS posting_execution_id, DROP COLUMN IF EXISTS journal_hash, DROP COLUMN IF EXISTS journal_purpose,
    DROP COLUMN IF EXISTS correction_kind, DROP COLUMN IF EXISTS reversal_reason, DROP COLUMN IF EXISTS copy_of_journal_id,
    DROP COLUMN IF EXISTS recurring_template_id, DROP COLUMN IF EXISTS withdrawn_at, DROP COLUMN IF EXISTS withdrawn_by_principal_id,
    DROP COLUMN IF EXISTS withdrawn_reason;
