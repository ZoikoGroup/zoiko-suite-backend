-- Atomic Linking columns required by docs/architecture doctrine §3.3 (Event
-- Linkage Keys) and the original spec's JournalEntry/JournalLine shapes:
-- every posting must carry, where applicable, the upstream event and
-- governance decision that caused it, and every line with a tax basis must
-- carry the immutable TaxLogicSnapshot it was computed from.
--
-- All four columns are nullable, not required: a manually-entered journal
-- (no upstream event, no governing decision) and a line with no tax
-- component (tax_code IS NULL) are both legitimate — Atomic Linking means
-- "carry the link when one exists", not "fabricate one when it doesn't".
-- No TaxLogicSnapshot-producing service exists yet anywhere in this
-- platform, so tax_logic_snapshot_id is currently always NULL in practice —
-- a documented v1 gap (same posture as chart_of_accounts), not an oversight.

ALTER TABLE journal_headers
    ADD COLUMN source_event_id        TEXT,
    ADD COLUMN governance_decision_id TEXT;

ALTER TABLE journal_lines
    ADD COLUMN tax_code              VARCHAR(64),
    ADD COLUMN tax_logic_snapshot_id TEXT;
