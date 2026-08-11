ALTER TABLE journal_lines
    DROP COLUMN IF EXISTS tax_code,
    DROP COLUMN IF EXISTS tax_logic_snapshot_id;

ALTER TABLE journal_headers
    DROP COLUMN IF EXISTS source_event_id,
    DROP COLUMN IF EXISTS governance_decision_id;
