-- 000005_add_rls.up.sql
-- Row-level security for commercial-account-svc (tracker row 11b).
--
-- Why this exists at all: the store package previously argued RLS was
-- unnecessary here because "this platform's pools connect as a Postgres
-- superuser, which unconditionally bypasses RLS". The superuser fact is
-- true and load-bearing — it is why every RLS test in this repo creates a
-- NOSUPERUSER NOBYPASSRLS role rather than trusting TEST_DATABASE_URL —
-- but it describes the test harness, not production, where the runtime
-- role is zoiko_app, a non-owner. The backstop was traded away on a
-- property of the test DSN, and the explicit filters it was traded for did
-- not hold (see row 11a: one of them disabled itself when the tenant
-- header was absent).
--
-- ── Three table classes ────────────────────────────────────────────────
--
-- This service's tables do NOT all carry a tenant column, and that is
-- correct. Making them uniform would be wrong in both directions.
--
-- 1. DIRECT (organization_id column):
--      commercial_accounts, memberships
--    Plain equality policy.
--
-- 2. PLATFORM SCOPE (no tenant column, and must not have one):
--      price_catalogs, plans, entitlement_limits
--    doc7 §U1 — an approved catalog is never edited in place; a change is a
--    new version. Every tenant reads the same published catalog, so there
--    is no tenant dimension to enforce. These get NO policy and NO RLS.
--    Adding either would break catalog reads for every tenant. Pinned by
--    TestRLS_PlatformTablesHaveNoPolicy.
--
-- 3. DERIVED (no tenant column; the organization is reachable by FK):
--      via commercial_account_id — commercial_subscriptions,
--        contract_entitlement_overlays, billing_source_transfers
--      via subscription_id      — evaluation_programs,
--        commercial_usage_meter_events, subscription_change_requests,
--        subscription_status_events
--    Subquery policies. See the caveat below.
--
-- ── The subquery caveat, which is the load-bearing part ────────────────
--
-- A policy's subquery is itself subject to RLS on the table it reads. That
-- has two consequences worth stating explicitly, because neither is
-- obvious and both are easy to break later:
--
--   (a) It works in our favour. A level-2 policy reads
--       commercial_accounts, which has its own policy, so the tenant
--       filter applies twice over. A level-3 policy reads
--       commercial_subscriptions, whose policy in turn reads
--       commercial_accounts — a two-deep chain that resolves correctly.
--
--   (b) It couples the children to the parent, and the two ways of
--       "removing the parent policy" behave in OPPOSITE directions.
--       Verified against Postgres 16, because the intuitive guess here is
--       wrong:
--
--         DROP POLICY on commercial_accounts   → fails CLOSED.
--           RLS stays enabled with no applicable policy, which Postgres
--           reads as deny-all, so the subquery returns the empty set and
--           the children get MORE restrictive. Even the owning
--           organization loses its own child rows.
--
--         DISABLE ROW LEVEL SECURITY on it     → fails OPEN.
--           The subquery now sees every account, and all seven derived
--           tables widen at once.
--
--       So a single ALTER TABLE on ONE table is a seven-table breach,
--       while dropping that table's policy is merely an outage. The two
--       look equally innocuous in a diff. Both directions are asserted by
--       TestRLS_ParentPolicyCoupling, which is what caught the wrong
--       prediction this comment originally carried.
--
-- The policies are written as explicit subqueries rather than via a shared
-- SQL helper function on purpose. A helper reads better, but it adds an
-- indirection that a reviewer of a security policy has to go and chase,
-- and the obvious way to make such a helper perform (SECURITY DEFINER) is
-- a well-known way to accidentally bypass the very policy it supports.
--
-- ── Type and empty-string handling ────────────────────────────────────
--
-- organization_id is UUID, so the GUC is compared as ::text rather than
-- casting the setting to uuid: app.tenant_id can legitimately be the empty
-- string (TenantFromContext returns "" when no verified tenant is present,
-- deliberately, rather than a fabricated default), and ''::uuid raises
-- invalid input syntax — which would turn a missing tenant into a 500
-- instead of an empty result. Comparing as text makes "" match no row,
-- which is the intended fail-closed behaviour.

-- ── Class 1: direct ───────────────────────────────────────────────────

ALTER TABLE commercial_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE commercial_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON commercial_accounts
    FOR ALL
    USING (organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON memberships
    FOR ALL
    USING (organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

-- ── Class 3a: derived via commercial_account_id ───────────────────────

ALTER TABLE commercial_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE commercial_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON commercial_subscriptions
    FOR ALL
    USING (commercial_account_id IN (SELECT commercial_account_id FROM commercial_accounts))
    WITH CHECK (commercial_account_id IN (SELECT commercial_account_id FROM commercial_accounts));

ALTER TABLE contract_entitlement_overlays ENABLE ROW LEVEL SECURITY;
ALTER TABLE contract_entitlement_overlays FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON contract_entitlement_overlays
    FOR ALL
    USING (commercial_account_id IN (SELECT commercial_account_id FROM commercial_accounts))
    WITH CHECK (commercial_account_id IN (SELECT commercial_account_id FROM commercial_accounts));

ALTER TABLE billing_source_transfers ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing_source_transfers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON billing_source_transfers
    FOR ALL
    USING (commercial_account_id IN (SELECT commercial_account_id FROM commercial_accounts))
    WITH CHECK (commercial_account_id IN (SELECT commercial_account_id FROM commercial_accounts));

-- ── Class 3b: derived via subscription_id (two levels) ────────────────
--
-- The inner SELECT reads commercial_subscriptions, which is itself
-- policy-filtered by the block above — so these resolve through two
-- policies. No explicit organization predicate is repeated here; adding
-- one would duplicate a boundary that the chain already enforces and
-- create a second place to keep correct.

ALTER TABLE evaluation_programs ENABLE ROW LEVEL SECURITY;
ALTER TABLE evaluation_programs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evaluation_programs
    FOR ALL
    USING (subscription_id IN (SELECT subscription_id FROM commercial_subscriptions))
    WITH CHECK (subscription_id IN (SELECT subscription_id FROM commercial_subscriptions));

ALTER TABLE commercial_usage_meter_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE commercial_usage_meter_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON commercial_usage_meter_events
    FOR ALL
    USING (subscription_id IN (SELECT subscription_id FROM commercial_subscriptions))
    WITH CHECK (subscription_id IN (SELECT subscription_id FROM commercial_subscriptions));

ALTER TABLE subscription_change_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_change_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON subscription_change_requests
    FOR ALL
    USING (subscription_id IN (SELECT subscription_id FROM commercial_subscriptions))
    WITH CHECK (subscription_id IN (SELECT subscription_id FROM commercial_subscriptions));

ALTER TABLE subscription_status_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_status_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON subscription_status_events
    FOR ALL
    USING (subscription_id IN (SELECT subscription_id FROM commercial_subscriptions))
    WITH CHECK (subscription_id IN (SELECT subscription_id FROM commercial_subscriptions));

-- ── Supporting indexes for the subquery policies ──────────────────────
--
-- Every derived-table read now evaluates an IN (SELECT …) against the
-- parent. commercial_accounts(organization_id) is already indexed
-- (idx_commercial_accounts_organization, plus a unique index), and
-- commercial_subscriptions.subscription_id is its primary key. The one
-- lookup with no index behind it is the level-2 join column.
CREATE INDEX IF NOT EXISTS idx_commercial_subscriptions_account
    ON commercial_subscriptions (commercial_account_id);
CREATE INDEX IF NOT EXISTS idx_contract_overlays_account
    ON contract_entitlement_overlays (commercial_account_id);
CREATE INDEX IF NOT EXISTS idx_billing_source_transfers_account
    ON billing_source_transfers (commercial_account_id);
