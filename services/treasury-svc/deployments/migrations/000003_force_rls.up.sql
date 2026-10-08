-- Priority 3: Add FORCE ROW LEVEL SECURITY to all tenant-scoped tables.
-- RLS was already ENABLED with policies from migration 000001.
-- FORCE ensures the policy also applies to the table owner role, providing
-- defence-in-depth if the table owner ever connects directly (e.g. manual psql).
-- Under the normal runtime role (zoiko_app, NOSUPERUSER NOBYPASSRLS) this has
-- no effect on live traffic -- it is insurance against a future regression.
-- See docs/architecture/backend-completion-tracker.md Priority 3.

ALTER TABLE bank_accounts FORCE ROW LEVEL SECURITY;
ALTER TABLE cash_balances FORCE ROW LEVEL SECURITY;
ALTER TABLE liquidity_thresholds FORCE ROW LEVEL SECURITY;
ALTER TABLE transfers FORCE ROW LEVEL SECURITY;