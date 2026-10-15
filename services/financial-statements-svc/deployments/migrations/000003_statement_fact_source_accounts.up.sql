-- Migration: 000003_statement_fact_source_accounts.up.sql
--
-- Statement line aggregation (MappingVersionV2, SPEC_DEVIATIONS A3/A4): one
-- statement_facts row may now sum several snapshot accounts. source_accounts
-- records every one of them with its own balance, as the snapshot stated it,
-- so ExplainReportedFact can name all of them rather than only the
-- representative account_code.
--
-- Existing rows (built per account, before aggregation) default to '[]';
-- the service treats an empty list as "this fact is its own single account".
-- Additive and non-locking beyond a brief ACCESS EXCLUSIVE for the catalog
-- change: a constant DEFAULT does not rewrite the table on Postgres 11+.

ALTER TABLE statement_facts
    ADD COLUMN source_accounts JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE statement_facts
    ADD CONSTRAINT statement_facts_source_accounts_is_array
    CHECK (jsonb_typeof(source_accounts) = 'array');
