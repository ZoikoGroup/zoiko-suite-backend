-- Migration: 000006_mapping_keys.up.sql
--
-- ACC-02: the general ledger resolves posting accounts from current,
-- versioned account mappings. A subledger may name a governed mapping key,
-- never a raw GL account code. These columns persist the mapping key that
-- was used when a run/event was created, so a later mapping version change
-- cannot silently reinterpret historical postings.
--
-- The legacy *_account_code columns stay in place for rows created before
-- this migration. New rows leave them empty and use the mapping-key columns
-- below; the application never posts from the legacy columns.

ALTER TABLE depreciation_runs
    ADD COLUMN depreciation_expense_mapping_key VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN accumulated_depreciation_mapping_key VARCHAR(64) NOT NULL DEFAULT '';

ALTER TABLE asset_events
    ADD COLUMN debit_mapping_key VARCHAR(64),
    ADD COLUMN credit_mapping_key VARCHAR(64);
