-- 000002_add_rls.up.sql
-- Row-level security for kill-switch-registry-svc (tracker row 17).
--
-- ── Read this before changing the policy ──────────────────────────────
--
-- This is the one service in the tier where a NAIVE tenant policy is not
-- an outage or a leak but a SILENT SAFETY BYPASS. Getting it wrong makes
-- the platform's emergency stop report "not engaged" while it is engaged.
--
-- kill_switch_events.tenant_id is NULLABLE, and NULL is meaningful: it
-- means "not scoped to one tenant" — i.e. a platform-wide kill switch.
-- Same nullable-scope doctrine as plane, domain and provider_code, and as
-- policy_versions' tenant_id (see 000001_initial_schema.up.sql).
--
-- PgStore.ResolveKillSwitch answers "must this class of action stop right
-- now for this tenant" with, in part:
--
--     AND (tenant_id IS NULL OR tenant_id = $4::uuid)
--
-- That IS NULL branch is load-bearing. A tenant-scoped caller MUST see
-- platform-wide switches, because a platform-wide ENGAGE is precisely the
-- case where every tenant's actions must stop.
--
-- So a policy written as the obvious
--
--     USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
--
-- would hide every tenant_id IS NULL row from every tenant-scoped
-- resolution. ResolveKillSwitch would then find no ENGAGE and answer
-- "not engaged" — during an incident, for an action class that has been
-- globally stopped. The caller proceeds to charge, or run the automation,
-- or publish the claim. Nothing errors. Nothing logs. doc7 §32.1 calls
-- this control "privileged, logged, approval-controlled"; a policy like
-- that turns it off for everyone while leaving the UI showing it on.
--
-- Hence the IS NULL branch below, and hence
-- TestRLS_PlatformWideSwitchStaysVisibleToTenants, which is an
-- OVER-RESTRICTIVE negative control: it fails if the policy is tightened
-- in the "obvious" direction. Same failure mode as
-- configuration-feature-flag-svc's global defaults (Priority 1 row 8), but
-- there the consequence was a wrong config value; here it is an unenforced
-- emergency stop.
--
-- ── WITH CHECK, and what actually guards a platform-wide ENGAGE ───────
--
-- WITH CHECK carries the same IS NULL branch, which means RLS permits any
-- caller to insert a tenant_id IS NULL row — that is, to engage a
-- PLATFORM-WIDE kill switch. Stated plainly rather than left implicit:
-- RLS is NOT the control for that, and must not be mistaken for it.
--
-- The real control is the handler's per-scope authorization. Handler.
-- authorize falls back to platformScopeID when the request names no
-- tenant, so engaging a platform-wide switch requires a KILL_SWITCH_ENGAGE
-- grant at platform scope from authorization-svc, which defaults to DENIED
-- with basis "no_grant". Migration 000001's append-only design does the
-- rest: an ENGAGE is a new row, never an UPDATE, so a forged event cannot
-- erase the history that shows it.
--
-- The alternative — an app.platform_scope GUC gating NULL-tenant inserts,
-- as audit-event-store-svc uses for its Kafka writer — was considered and
-- rejected here: it would put the platform-wide ENGAGE path behind a
-- second, store-level switch that an incident responder could not see, and
-- during an incident a control nobody can find is worse than one whose
-- guard is a legible authz grant.
--
-- ── Type handling ────────────────────────────────────────────────────
--
-- tenant_id is UUID, compared as ::text against the GUC rather than
-- casting the GUC to uuid: app.tenant_id is legitimately the empty string
-- when a caller has no verified tenant (a platform-level resolution — see
-- the middleware doc comment), and ''::uuid raises invalid input syntax.
-- Comparing as text makes "" match no tenant-specific row while STILL
-- matching the IS NULL branch, which is exactly right: a platform-level
-- caller should see platform-wide switches and nobody's tenant-specific
-- ones.

ALTER TABLE kill_switch_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE kill_switch_events FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON kill_switch_events
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    );
