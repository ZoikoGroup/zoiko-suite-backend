-- Migration: 000036_account_intercompany_classification.up.sql
--
-- intercompany-accounting-svc SPEC_DEVIATIONS A5: "There is no
-- intercompany-account resolution ('Server-resolved: intercompany
-- accounts')... Needs an intercompany account designation from the chart of
-- accounts (ACC-01/02)." ACC-01's own "Server-resolved / derived context"
-- line names control-account classification; ACC-11 §14 names intercompany
-- accounts the same way. This adds that second, independent classification
-- to the chart, mirroring is_control_account's exact shape (migration
-- 000007) rather than inventing a new kind of flag: a plain boolean on the
-- account, carried into every append-only account_versions row exactly as
-- is_control_account already is.
--
-- Additive and backward compatible: every existing account defaults to
-- FALSE (not intercompany), which is the same answer ResolvePostingEligibility
-- and GetJournal already give for an account nothing has classified.
ALTER TABLE chart_of_accounts
    ADD COLUMN is_intercompany_account BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE account_versions
    ADD COLUMN is_intercompany_account BOOLEAN NOT NULL DEFAULT FALSE;

-- account_change_approvals stores the proposed account state as a JSONB
-- "amendment" blob (migration 000031), not individual columns, so no schema
-- change is needed there: domain.Account already round-trips through it.
