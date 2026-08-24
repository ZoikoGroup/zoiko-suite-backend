-- 000002_add_rls.up.sql
-- Row-level security for retention-registry-svc (tracker row 18).
--
-- ── Read this before changing the policy ──────────────────────────────
--
-- Both tables have a NULLABLE tenant_id where NULL means "applies
-- platform-wide, not one tenant" — the same nullable-scope doctrine as
-- kill-switch-registry-svc (tracker row 17) and configuration-feature-flag-
-- svc's global defaults (row 8). As in row 17, the obvious policy is not a
-- tightening but a silent failure. Unlike row 17, the failure here is
-- IRREVERSIBLE.
--
-- This service answers exactly one question for every other service that
-- owns deletable data: "is it safe to delete/export/migrate this right
-- now?" (see the resolve endpoint). Both halves of that answer depend on
-- an IS NULL branch:
--
--   FindApplicableRetentionPolicy:
--       AND (tenant_id IS NULL OR tenant_id = $3::uuid)
--   FindActiveHoldForScope:
--       AND (tenant_id IS NULL OR tenant_id = $2::uuid)
--
-- Tighten the policy to a plain tenant equality and both platform-wide
-- rules become invisible to every tenant-scoped caller:
--
--   * A platform-wide RETENTION POLICY sets min_retention_days for a
--     record class. Hidden, Resolve reports no applicable policy, and the
--     caller concludes it may delete. doc7 §J2: "No automatic destructive
--     deletion of governed records."
--
--   * A platform-wide LEGAL HOLD blocks deletion/export/migration until
--     authorized release. Hidden, Resolve reports no hold, and the
--     deletion path proceeds on records under a legal preservation
--     obligation. doc7 §J3: "Hold checks occur in deletion/export/
--     migration paths." Destroying records under hold is spoliation, and
--     unlike a missed kill switch it cannot be undone by re-engaging
--     anything.
--
-- So the difference between this policy and the "obvious" one is the
-- difference between a retained record and a destroyed one. That is why
-- TestRLS_PlatformWideRuleStaysVisibleToTenants is an OVER-RESTRICTIVE
-- negative control: it fails if the policy is tightened in the direction
-- that looks like hardening.
--
-- ── WITH CHECK, and what actually guards a platform-wide rule ─────────
--
-- WITH CHECK carries the same IS NULL branch, so RLS permits any caller to
-- insert a tenant_id IS NULL row — a platform-wide retention policy or a
-- platform-wide legal hold. Stated plainly rather than left implicit: RLS
-- is NOT the control there.
--
-- The controls are in the handler, and they are per-scope:
--   * CreateRetentionPolicy and CreateLegalHold authorize against the
--     request's own tenant_id, falling back to platformScopeID when it is
--     absent — so creating a platform-wide rule needs a platform-scope
--     grant from authorization-svc (default DENIED, basis "no_grant").
--   * ReleaseLegalHold fetches the hold FIRST and authorizes against that
--     hold's own tenant, so releasing another tenant's hold needs a grant
--     for that tenant. This is why the unscoped store-level UPDATE was a
--     defence-in-depth gap rather than an open door — worth being precise
--     about, because "anyone could release any legal hold" would have been
--     the dramatic claim and it is not true.
--
-- Note also that a platform-wide legal hold being insertable by anyone is
-- the SAFE direction of that particular asymmetry: an unauthorized extra
-- hold blocks deletion, it does not permit one. The dangerous direction is
-- release, and release is authz-gated per hold.
--
-- ── Type handling ────────────────────────────────────────────────────
--
-- tenant_id is UUID, compared as ::text against the GUC rather than
-- casting the GUC to uuid: app.tenant_id is legitimately the empty string
-- for a platform-level question, and ''::uuid raises invalid input syntax.
-- Comparing as text makes "" match no tenant-specific row while still
-- matching the IS NULL branch — correct, because a platform-level caller
-- must see platform-wide retention rules and holds.

ALTER TABLE retention_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE retention_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON retention_policies
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    );

ALTER TABLE legal_holds ENABLE ROW LEVEL SECURITY;
ALTER TABLE legal_holds FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON legal_holds
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    );
